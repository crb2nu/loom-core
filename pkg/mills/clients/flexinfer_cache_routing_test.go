package clients

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/agentloop"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// researchNotesBody is a plain (non-reasoning) research completion: non-empty
// content, so the weaver returns after exactly one call and the recorded
// headers describe that one call.
const researchNotesBody = `{
  "model": "qwen3-8b-instruct",
  "choices": [
    {"message": {"role": "assistant", "content": "Research notes: the routing key rides on the request, not the client."}}
  ],
  "usage": {"prompt_tokens": 120, "completion_tokens": 40, "total_tokens": 160}
}`

// newHeaderCapturingClient returns a client whose transport records the headers
// of every /v1/chat/completions request and replies with body.
func newHeaderCapturingClient(t *testing.T, body string, headers *[]http.Header) *FlexInferClient {
	t.Helper()
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", WeaverModel: "qwen3-8b-instruct", DisableRegistryFallbacks: true})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		*headers = append(*headers, req.Header.Clone())
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     make(http.Header),
		}, nil
	}))
	return cli
}

// TestWeaverResearch_SetsPerItemCacheKey is the routing contract: the research
// prompt leads with the item's journal render, so its request must pin a
// per-item routing key or two items' deep prefixes compete for one replica's
// cache on a multi-replica lane.
func TestWeaverResearch_SetsPerItemCacheKey(t *testing.T) {
	var headers []http.Header
	cli := newHeaderCapturingClient(t, researchNotesBody, &headers)
	w := &WeaverClient{Client: cli, MaxTokens: 512}

	if _, err := w.Research(context.Background(), pipeline.WeaverRequest{
		RunID:     "RUN-1",
		BacklogID: "BL-2026-07-26-001",
		Prompt:    "research this",
	}); err != nil {
		t.Fatalf("research: %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(headers))
	}
	if got, want := headers[0].Get(agentloop.HeaderCacheKey), "mills-item:BL-2026-07-26-001"; got != want {
		t.Errorf("cache key header = %q, want %q", got, want)
	}
}

// TestWeaverResearch_NoCacheKeyWithoutItemID: absent an item id there is no
// owner to pin routing to, and a bare prefix would collide across every such
// call. The header must simply not be sent.
func TestWeaverResearch_NoCacheKeyWithoutItemID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		var headers []http.Header
		cli := newHeaderCapturingClient(t, researchNotesBody, &headers)
		w := &WeaverClient{Client: cli, MaxTokens: 512}

		if _, err := w.Research(context.Background(), pipeline.WeaverRequest{RunID: "RUN-1", BacklogID: id, Prompt: "p"}); err != nil {
			t.Fatalf("research(%q): %v", id, err)
		}
		if len(headers) != 1 {
			t.Fatalf("expected 1 upstream call, got %d", len(headers))
		}
		if _, ok := headers[0][http.CanonicalHeaderKey(agentloop.HeaderCacheKey)]; ok {
			t.Errorf("backlog id %q: cache key header present (%q), want absent",
				id, headers[0].Get(agentloop.HeaderCacheKey))
		}
	}
}

// TestJudgeChat_NoCacheKey: the routing key is per-request, never a client-wide
// SetHeader. One FlexInferClient fronts the judge and the weaver, so a judge
// call on the same client must carry no key.
func TestJudgeChat_NoCacheKey(t *testing.T) {
	var headers []http.Header
	cli := newHeaderCapturingClient(t, successBody, &headers)
	judge := NewRubricJudge(cli)

	if _, err := judge.Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{
		Item:         &store.BacklogItem{ID: "BL-X", Title: "x"},
		FilesChanged: []string{"foo.go"},
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if len(headers) == 0 {
		t.Fatal("expected at least 1 upstream call")
	}
	for i, h := range headers {
		if _, ok := h[http.CanonicalHeaderKey(agentloop.HeaderCacheKey)]; ok {
			t.Errorf("judge call %d carried a cache key header %q; the seam must stay per-request",
				i, h.Get(agentloop.HeaderCacheKey))
		}
	}
}

// TestChatRequest_PreservesContentTypeAndAuth guards the switch from
// httpclient.Post to a hand-built request: Content-Type must still be set and
// the client-global Authorization must still be applied by the transport
// wrapper SetHeader installs. Runs against a real httptest server rather than
// SetTransport, because SetTransport REPLACES that wrapper and would hide the
// header this test exists to check.
func TestChatRequest_PreservesContentTypeAndAuth(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, researchNotesBody)
	}))
	defer srv.Close()

	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: srv.URL, Token: "sekret", DisableRegistryFallbacks: true, WeaverModel: "m"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, _, err := cli.chatWithOptions(context.Background(), "m", "p", 64, chatOptions{cacheKey: "mills-item:BL-9"}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if ct := got.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := got.Get("Authorization"); auth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want Bearer sekret", auth)
	}
	if key := got.Get(agentloop.HeaderCacheKey); key != "mills-item:BL-9" {
		t.Errorf("%s = %q, want mills-item:BL-9", agentloop.HeaderCacheKey, key)
	}
}

// TestChatRequest_RetryReplaysBody: httpclient.Client retries 5xx by replaying
// req.GetBody. The hand-built request must populate it, or a retried call would
// POST an empty body. HTTP_RETRIES is read by httpclient.DefaultConfig at
// construction (the shared client defaults to no retries).
func TestChatRequest_RetryReplaysBody(t *testing.T) {
	t.Setenv("HTTP_RETRIES", "1")
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", DisableRegistryFallbacks: true, WeaverModel: "m"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls int32
	var bodies []string
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(buf))
		if atomic.AddInt32(&calls, 1) == 1 {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewBufferString("boom")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(researchNotesBody)), Header: make(http.Header)}, nil
	}))
	if _, _, err := cli.chatWithOptions(context.Background(), "m", "prompt-text", 64, chatOptions{cacheKey: "mills-item:BL-1"}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(bodies) < 2 {
		t.Fatalf("expected a retry after 500, got %d call(s)", len(bodies))
	}
	if bodies[0] == "" || bodies[0] != bodies[1] {
		t.Errorf("retry body not replayed: first=%q second=%q", bodies[0], bodies[1])
	}
}

func TestItemCacheKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"BL-1", "mills-item:BL-1"},
		{"  BL-2  ", "mills-item:BL-2"},
		{"", ""},
		{"   ", ""},
	} {
		if got := itemCacheKey(tc.in); got != tc.want {
			t.Errorf("itemCacheKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

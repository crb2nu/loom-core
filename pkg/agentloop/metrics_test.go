package agentloop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTurnMetrics(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderUpstreamMs, "1412")
	h.Set(HeaderFinishReason, "stop")
	h.Set(HeaderCachedTokens, "5000")
	m := parseTurnMetrics(h, 5154, 0)
	require.Equal(t, int64(1412), m.UpstreamMs)
	require.Equal(t, 5154, m.PromptTokens)
	require.NotNil(t, m.CachedTokens)
	require.Equal(t, 5000, *m.CachedTokens)
	require.NotNil(t, m.PrefixHitRatio)
	require.InDelta(t, 5000.0/5154.0, *m.PrefixHitRatio, 1e-9)
	require.Nil(t, m.PrefixCacheHitRate, "engine-reported rate absent when header not set")

	// CachedTokens absent (the gemma4 fallback) -> nil, not zero. But the
	// proxy-scraped engine rate IS present (the row-195 follow-up): the client
	// now reports the hit ratio directly even without cached_tokens.
	h2 := http.Header{}
	h2.Set(HeaderPromptTokens, "8000")
	h2.Set(HeaderPrefixHitRate, "0.9300")
	m2 := parseTurnMetrics(h2, 0, 0)
	require.Nil(t, m2.CachedTokens)
	require.Nil(t, m2.PrefixHitRatio)
	require.Equal(t, 8000, m2.PromptTokens, "falls back to header when usage absent")
	require.NotNil(t, m2.PrefixCacheHitRate, "engine-reported rate parsed from proxy header")
	require.InDelta(t, 0.93, *m2.PrefixCacheHitRate, 1e-9)
}

// TestParseTurnMetricsPrefersBodyCachedTokens covers the lane reached without
// the FlexInfer proxy in front: nothing synthesizes X-Flexinfer-Cached-Tokens,
// so the body's usage.prompt_tokens_details is the only source. It also pins
// the precedence when both are present — the body wins, because the header is
// only the proxy restating what the engine said.
func TestParseTurnMetricsPrefersBodyCachedTokens(t *testing.T) {
	// Body only, no proxy header at all.
	bodyOnly := parseTurnMetrics(http.Header{}, 4096, 3968)
	require.NotNil(t, bodyOnly.CachedTokens, "body-reported cached tokens must be captured without the proxy header")
	require.Equal(t, 3968, *bodyOnly.CachedTokens)
	require.NotNil(t, bodyOnly.PrefixHitRatio)
	require.InDelta(t, 3968.0/4096.0, *bodyOnly.PrefixHitRatio, 1e-9)

	// Both present and disagreeing: the engine's own number wins.
	h := http.Header{}
	h.Set(HeaderCachedTokens, "100")
	both := parseTurnMetrics(h, 4096, 3968)
	require.Equal(t, 3968, *both.CachedTokens, "body must win over the proxy header")

	// Neither reported stays nil, so "unmeasured" never masquerades as a 0% hit.
	neither := parseTurnMetrics(http.Header{}, 4096, 0)
	require.Nil(t, neither.CachedTokens)
	require.Nil(t, neither.PrefixHitRatio)
}

// TestChatClientParsesBodyCachedTokens is the end-to-end version: an engine
// that reports prompt_tokens_details and no proxy headers at all.
func TestChatClientParsesBodyCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gemma4","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4096,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":3968}}}`))
	}))
	defer srv.Close()

	c, err := NewChatClient(ChatClientConfig{Endpoint: srv.URL, Model: "gemma4"})
	require.NoError(t, err)
	_, metrics, err := c.Complete(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, nil, 48)
	require.NoError(t, err)
	require.Equal(t, 4096, metrics.PromptTokens)
	require.NotNil(t, metrics.CachedTokens, "engine-reported cached_tokens must reach TurnMetrics")
	require.Equal(t, 3968, *metrics.CachedTokens)
	require.NotNil(t, metrics.PrefixHitRatio)
	require.InDelta(t, 0.96875, *metrics.PrefixHitRatio, 1e-9)
}

func TestChatClientSendsOptInAndParsesRate(t *testing.T) {
	var gotCacheKey, gotWantPrefixHit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCacheKey = r.Header.Get(HeaderCacheKey)
		gotWantPrefixHit = r.Header.Get(HeaderWantPrefixHit)
		w.Header().Set(HeaderUpstreamMs, "1412")
		w.Header().Set(HeaderFinishReason, "stop")
		// gemma4 omits cached_tokens but the proxy supplies the scraped rate.
		w.Header().Set(HeaderPrefixHitRate, "0.9300")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5154}}`))
	}))
	defer srv.Close()

	c, err := NewChatClient(ChatClientConfig{Endpoint: srv.URL, Model: "m", CacheKey: "sess-42", WantPrefixHit: true})
	require.NoError(t, err)
	msg, metrics, err := c.Complete(context.Background(),
		[]Message{{Role: RoleSystem, Content: "SYS"}}, nil, 48)
	require.NoError(t, err)
	require.Equal(t, "hi", msg.Content)
	require.Equal(t, "sess-42", gotCacheKey, "cache key must pin prefix routing")
	require.Equal(t, "1", gotWantPrefixHit, "opt-in header sent when WantPrefixHit")
	require.Equal(t, int64(1412), metrics.UpstreamMs)
	require.NotNil(t, metrics.PrefixCacheHitRate, "engine-reported rate parsed even without cached_tokens")
	require.InDelta(t, 0.93, *metrics.PrefixCacheHitRate, 1e-9)
}

func TestChatClientOmitsOptInByDefault(t *testing.T) {
	gotWantPrefixHit := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWantPrefixHit = r.Header.Get(HeaderWantPrefixHit)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10}}`))
	}))
	defer srv.Close()

	c, err := NewChatClient(ChatClientConfig{Endpoint: srv.URL, Model: "m"}) // WantPrefixHit false
	require.NoError(t, err)
	_, _, err = c.Complete(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, nil, 48)
	require.NoError(t, err)
	require.Empty(t, gotWantPrefixHit, "no opt-in header on the zero-cost default path")
}

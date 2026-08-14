package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// SelfCheckResult is one named assertion in the offline self-check.
type SelfCheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// SelfCheckReport is the structured outcome of SelfCheck. Passed is the AND
// of every check. It is JSON-serialisable so the MCP tool can return it
// verbatim — the offline gate, mirroring the CLI's --self-check.
type SelfCheckReport struct {
	Passed bool              `json:"passed"`
	Checks []SelfCheckResult `json:"checks"`
}

// SelfCheck exercises the whole engine offline: a canned chat server that
// mimics the proxy (flexinfer headers + a scripted tool-call→final dialogue),
// a real temp-dir file the read_file tool actually reads, and assertions on
// the append-only prefix invariant, header parsing, real tool execution, the
// final answer, the path-jail, and the budget arithmetic. No cluster
// required. It returns (nil, err) only on infrastructure failure (temp dir,
// server); assertion outcomes are reported in the *SelfCheckReport.
func SelfCheck(ctx context.Context) (*SelfCheckReport, error) {
	dir, err := os.MkdirTemp("", "agent-loop-selfcheck-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	const fileBody = "F4 append-only tool loop: prefix cache makes tool history a sunk cost.\n"
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(fileBody), 0o644); err != nil {
		return nil, err
	}

	srv, seen := cannedChatServer()
	defer srv.Close()

	tools, err := FSTools(dir)
	if err != nil {
		return nil, fmt.Errorf("FSTools: %w", err)
	}
	reg, err := NewRegistry(tools...)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	client, err := NewChatClient(ChatClientConfig{
		Endpoint: srv.URL, Model: "selfcheck-model", CacheKey: "selfcheck-session",
	})
	if err != nil {
		return nil, err
	}
	eng := &Engine{
		Client:       client,
		Registry:     reg,
		Budget:       Budget{MaxModelLen: 20480, SystemTokens: 100, OutputReserve: 48},
		MaxRounds:    8,
		OutputTokens: 48,
		ToolTimeout:  5 * time.Second,
	}

	conv := NewConversation("You are a self-check agent.")
	res, err := eng.Run(ctx, conv, "Read hello.txt and report its content.")
	if err != nil {
		return nil, fmt.Errorf("engine run: %w", err)
	}

	rep := &SelfCheckReport{Passed: true}
	add := func(name string, err error) {
		r := SelfCheckResult{Name: name, Passed: err == nil}
		if err != nil {
			r.Detail = err.Error()
			rep.Passed = false
		}
		rep.Checks = append(rep.Checks, r)
	}

	add("reaches_final_via_real_tool", assertResult(res, fileBody))
	add("append_only_prefix_extension", assertAppendOnly(seen.slices()))
	add("metrics_parsed_from_headers", assertMetrics(res))
	add("path_jail_rejects_escape", assertPathJail(reg))
	add("budget_arithmetic", assertBudget(eng.Budget))
	return rep, nil
}

// assertResult checks the loop reached a tool-call-free final answer after
// really executing the read_file tool (the file body must appear in the
// recorded tool result).
func assertResult(res *Result, fileBody string) error {
	if res.Stopped != StopFinal {
		return fmt.Errorf("stopped=%q want %q", res.Stopped, StopFinal)
	}
	if len(res.Rounds) != 2 {
		return fmt.Errorf("rounds=%d want 2 (one tool round, one final)", len(res.Rounds))
	}
	if len(res.Rounds[0].ToolCalls) != 1 {
		return fmt.Errorf("round 0 tool calls=%d want 1", len(res.Rounds[0].ToolCalls))
	}
	tc := res.Rounds[0].ToolCalls[0]
	if tc.Err != "" {
		return fmt.Errorf("tool error: %s", tc.Err)
	}
	if tc.Result != fileBody {
		return fmt.Errorf("tool result=%q want the real file body %q", tc.Result, fileBody)
	}
	if res.Answer == "" {
		return fmt.Errorf("empty final answer")
	}
	return nil
}

// assertAppendOnly verifies each request the engine sent was a strict
// prefix-extension of the previous one — the cache-paying invariant.
func assertAppendOnly(reqs [][]Message) error {
	if len(reqs) < 2 {
		return fmt.Errorf("only %d requests captured; need ≥2 to check append-only", len(reqs))
	}
	for r := 1; r < len(reqs); r++ {
		prev, cur := reqs[r-1], reqs[r]
		if len(cur) < len(prev) {
			return fmt.Errorf("request %d shrank (%d < %d) — not append-only", r, len(cur), len(prev))
		}
		for i := range prev {
			if !messagesEqual(prev[i], cur[i]) {
				return fmt.Errorf("request %d diverged at message %d — prefix busted", r, i)
			}
		}
	}
	return nil
}

func assertMetrics(res *Result) error {
	if len(res.Rounds) == 0 {
		return fmt.Errorf("no rounds recorded")
	}
	m := res.Rounds[0].Metrics
	if m.UpstreamMs <= 0 {
		return fmt.Errorf("upstream_ms not parsed from header (got %d)", m.UpstreamMs)
	}
	if m.PromptTokens <= 0 {
		return fmt.Errorf("prompt_tokens not parsed (got %d)", m.PromptTokens)
	}
	if m.PrefixHitRatio == nil {
		return fmt.Errorf("prefix_hit_ratio nil — cached-tokens header path not exercised")
	}
	return nil
}

// assertPathJail confirms the read-only tool refuses a path that escapes the
// working directory (returns an error rather than reading outside root).
func assertPathJail(reg *Registry) error {
	tool, ok := reg.Get("read_file")
	if !ok {
		return fmt.Errorf("read_file tool missing")
	}
	if _, err := tool.Invoke(context.Background(), `{"path":"../../../../etc/passwd"}`); err == nil {
		return fmt.Errorf("path jail breached: ../etc/passwd was allowed")
	}
	return nil
}

func assertBudget(b Budget) error {
	if u := b.Usable(); u != b.MaxModelLen-b.SystemTokens-b.OutputReserve {
		return fmt.Errorf("usable=%d want %d", u, b.MaxModelLen-b.SystemTokens-b.OutputReserve)
	}
	if be := b.Check(b.PromptCeiling() + 1); be == nil {
		return fmt.Errorf("budget did not flag an over-ceiling prompt")
	}
	return nil
}

func messagesEqual(a, b Message) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// captured records the message slices the canned server received, so the
// self-check can assert the append-only invariant from the wire side.
type captured struct {
	mu   sync.Mutex
	reqs [][]Message
}

func (c *captured) add(msgs []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]Message, len(msgs))
	copy(cp, msgs)
	c.reqs = append(c.reqs, cp)
}

func (c *captured) slices() [][]Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs
}

// cannedChatServer returns an httptest server that scripts a two-round
// dialogue: round 0 asks for read_file("hello.txt"); round 1 (once it has
// seen a tool result) returns a final answer. It always sets the flexinfer
// instrumentation headers so the metric-parse path is exercised.
func cannedChatServer() (*httptest.Server, *captured) {
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cap.add(body.Messages)

		hasToolResult := false
		for _, m := range body.Messages {
			if m.Role == RoleTool {
				hasToolResult = true
			}
		}

		w.Header().Set(HeaderUpstreamMs, "1400")
		w.Header().Set(HeaderPromptTokens, strconv.Itoa(500+100*len(body.Messages)))
		w.Header().Set(HeaderCachedTokens, "480")
		w.Header().Set("Content-Type", "application/json")

		var reply string
		if hasToolResult {
			w.Header().Set(HeaderFinishReason, "stop")
			reply = `{"choices":[{"message":{"role":"assistant","content":"hello.txt describes the F4 append-only tool loop."},"finish_reason":"stop"}],"usage":{"prompt_tokens":620}}`
		} else {
			w.Header().Set(HeaderFinishReason, "tool_calls")
			reply = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"hello.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":500}}`
		}
		_, _ = w.Write([]byte(reply))
	}))
	return srv, cap
}

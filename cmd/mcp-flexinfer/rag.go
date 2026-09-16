package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// handleRAG fronts the flexinfer-proxy /v1/rag route (the codebase-answer
// read-path service on the gfx906 retrieval plane: bge embed -> qdrant ->
// bge rerank, optionally -> chat synthesis).
//
// Default is retrieve_only=true: sub-second, returns the reranked chunk texts
// with path citations, and never touches a chat GPU lane — the calling agent
// synthesizes with its own LLM. Pass retrieve_only=false for a server-side
// cited answer (may cold-start the workhorse lane; can take minutes when the
// big-GPU lanes are contended).
func (f *flexinferServer) handleRAG(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	query := v.Required("query")
	collection := v.String("collection", "")
	retrieveOnly := v.Bool("retrieve_only", true)
	topK := v.Int("top_k", 0)
	topN := v.Int("top_n", 0)
	maxPerPath := v.Int("max_per_path", -1)
	proxyURL := v.String("proxy_url", f.proxyURL)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	if proxyURL == "" {
		return mcp.JSONResult(map[string]any{
			"ok":      false,
			"message": "proxy not configured; set FLEXINFER_PROXY_URL or pass proxy_url parameter",
		})
	}

	payload := map[string]any{
		"query":         query,
		"retrieve_only": retrieveOnly,
	}
	if collection != "" {
		payload["collection"] = collection
	}
	if topK > 0 {
		payload["top_k"] = topK
	}
	if topN > 0 {
		payload["top_n"] = topN
	}
	if maxPerPath >= 0 {
		payload["max_per_path"] = maxPerPath
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("marshal request: %w", err)), nil
	}

	url := strings.TrimRight(proxyURL, "/") + "/v1/rag"
	resp, err := f.httpClient.Post(ctx, url, "application/json", bytes.NewReader(body))
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("proxy POST %s: %w", url, err)), nil
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("read response: %w", err)), nil
	}
	if resp.StatusCode >= 400 {
		return mcp.ErrorResult(fmt.Errorf("proxy POST %s: HTTP %d: %s", url, resp.StatusCode, truncate(string(raw), 400))), nil
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return mcp.JSONResult(map[string]any{
			"ok":     true,
			"raw":    string(raw),
			"format": "text",
		})
	}

	return mcp.JSONResult(map[string]any{
		"ok":     true,
		"result": parsed,
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

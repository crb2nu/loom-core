package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

func TestStreamableHTTPChildFixture(t *testing.T) {
	if os.Getenv("CUSTOM_SERVER_TEST_CHILD") != "1" {
		return
	}
	server := mcp.NewServer("streamable-fixture", "1.0.0")
	server.AddTool(mcp.Tool{
		Name:        "echo",
		Description: "returns its text argument",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return mcp.TextResult(args["text"].(string)), nil
	})
	if err := server.Run(context.Background()); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStreamableHTTPInitializeListAndCall(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })
	t.Setenv("CUSTOM_SERVER_TEST_CHILD", "1")

	d := newDrainRegistry()
	h := newStreamableHTTPHandler("fixture", os.Args[0]+" -test.run=TestStreamableHTTPChildFixture --", d)

	initResp := postMCP(t, h, "", mcp.ProtocolVersion20250618,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if initResp.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body=%s", initResp.Code, initResp.Body.String())
	}
	sessionID := initResp.Header().Get(mcpSessionIDHeader)
	if !strings.HasPrefix(sessionID, "http-") {
		t.Fatalf("session header = %q", sessionID)
	}
	if got := initResp.Header().Get(mcpProtocolVersionHeader); got != mcp.ProtocolVersion20250618 {
		t.Fatalf("protocol header = %q", got)
	}
	if got := d.Len(); got != 1 {
		t.Fatalf("active drain sessions = %d, want 1", got)
	}

	listResp := postMCP(t, h, sessionID, mcp.ProtocolVersion20250618,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var listMsg mcp.Message
	decodeResponse(t, listResp, &listMsg)
	var list mcp.ToolsListResult
	if err := json.Unmarshal(listMsg.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("tools/list = %+v", list.Tools)
	}

	callResp := postMCP(t, h, sessionID, mcp.ProtocolVersion20250618,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"through-http"}}}`)
	var callMsg mcp.Message
	decodeResponse(t, callResp, &callMsg)
	var result mcp.CallToolResult
	if err := json.Unmarshal(callMsg.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "through-http" {
		t.Fatalf("tools/call result = %+v", result)
	}

	del := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	del.Header.Set(mcpSessionIDHeader, sessionID)
	delResp := httptest.NewRecorder()
	h.ServeHTTP(delResp, del)
	if delResp.Code != http.StatusOK || d.Len() != 0 {
		t.Fatalf("DELETE status=%d active=%d", delResp.Code, d.Len())
	}
}

func TestStreamableHTTPAcceptedProtocolVersions(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })
	h := newStreamableHTTPHandler("unused", "unused", newDrainRegistry())
	for _, version := range mcp.SupportedProtocolVersions {
		req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
		req.Header.Set(mcpProtocolVersionHeader, version)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if got := resp.Header().Get(mcpProtocolVersionHeader); got != version {
			t.Errorf("version %q returned %q", version, got)
		}
	}
}

func TestStreamableHTTPRejectsNewWorkWhileDraining(t *testing.T) {
	shuttingDown.Store(true)
	t.Cleanup(func() { shuttingDown.Store(false) })
	h := newStreamableHTTPHandler("unused", "unused", newDrainRegistry())
	resp := postMCP(t, h, "", mcp.ProtocolVersion20250618, `{}`)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.Code)
	}
}

func TestStreamableHTTPDoesNotClaimLegacyRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", newStreamableHTTPHandler("unused", "unused", newDrainRegistry()))
	for _, path := range []string{"/sse", "/messages", "/ws"} {
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusNotFound {
			t.Errorf("new handler claimed %s: status %d", path, resp.Code)
		}
	}
}

func postMCP(t *testing.T, h http.Handler, sessionID, version, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sessionID != "" {
		req.Header.Set(mcpSessionIDHeader, sessionID)
	}
	if version != "" {
		req.Header.Set(mcpProtocolVersionHeader, version)
	}
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	return resp
}

func decodeResponse(t *testing.T, resp *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}

func TestStreamableHTTPSharedDrainDeliversResult(t *testing.T) {
	shuttingDown.Store(false)
	defer shuttingDown.Store(false)
	s := newTestSharedSupervisor(t)
	sharedWSChild = s
	defer func() { sharedWSChild = nil }()
	marker := filepath.Join(t.TempDir(), "entered")
	t.Setenv("CUSTOM_SERVER_DRAIN_MARKER", marker)
	h := newStreamableHTTPHandler("devbox", s.command, newDrainRegistry())
	init := postMCP(t, h, "", mcp.ProtocolVersion20250618, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	id := init.Header().Get(mcpSessionIDHeader)
	if id == "" {
		t.Fatalf("missing session: %s", init.Body.String())
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- postMCP(t, h, id, mcp.ProtocolVersion20250618, `{"jsonrpc":"2.0","id":2,"method":"test/drain"}`)
	}()
	waitDrainMarker(t, marker)
	shuttingDown.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.beginDrain(ctx); close(done) }()
	select {
	case got := <-result:
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "test/drain") {
			t.Fatalf("lost HTTP result: %d %s", got.Code, got.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("HTTP result timed out")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("wrapper drain timed out")
	}
	rejected := postMCP(t, h, id, mcp.ProtocolVersion20250618, `{"jsonrpc":"2.0","id":3,"method":"tools/call"}`)
	if !strings.Contains(rejected.Body.String(), `"retryable":true`) {
		t.Fatalf("missing MCP retryable error: %s", rejected.Body.String())
	}
}

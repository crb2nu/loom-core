// Package mcptest drives an mcp.Server in-process over stdio pipes so a
// server binary's package can prove, without a subprocess, that it answers
// the client handshake and a tool call in a given environment. The
// all-servers integration smoke test covers the same ground with the built
// binaries; this is the unit-level half that runs on every `go test`.
package mcptest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

// Responses holds the three responses of one Drive session.
type Responses struct {
	Initialize *mcp.Message
	ToolsList  *mcp.Message
	ToolCall   *mcp.Message
}

// Drive runs srv over in-memory pipes and performs the client-side
// handshake a real MCP client does — initialize, notifications/initialized,
// tools/list — then one tools/call for tool with args. It fails the test on
// any transport error, a JSON-RPC error on initialize or tools/list, or a
// response that does not arrive within ten seconds. The server is shut down
// before Drive returns.
func Drive(t testing.TB, srv *mcp.Server, tool string, args map[string]any) Responses {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.RunWithIO(ctx, inR, outW) }()

	responses := make(chan *mcp.Message, 16)
	go func() {
		defer close(responses)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m mcp.Message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.ID == nil {
				continue // notifications and non-JSON lines are not responses
			}
			responses <- &m
		}
	}()

	send := func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("mcptest: marshal request: %v", err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatalf("mcptest: write request: %v", err)
		}
	}
	await := func(id int) *mcp.Message {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case m, ok := <-responses:
				if !ok {
					t.Fatalf("mcptest: server output closed before response %d", id)
				}
				if fmt.Sprint(m.ID) == fmt.Sprint(id) {
					return m
				}
			case <-deadline:
				t.Fatalf("mcptest: no response for request %d within 10s", id)
			}
		}
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcptest", "version": "0"},
	}})
	initialize := await(1)
	if initialize.Error != nil {
		t.Fatalf("mcptest: initialize failed: %+v", initialize.Error)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	list := await(2)
	if list.Error != nil {
		t.Fatalf("mcptest: tools/list failed: %+v", list.Error)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	call := await(3)

	cancel()
	_ = inW.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Log("mcptest: server did not stop within 5s after cancel")
	}
	_ = outW.Close()
	_ = outR.Close()

	return Responses{Initialize: initialize, ToolsList: list, ToolCall: call}
}

// ToolNames decodes a tools/list response into the advertised tool names.
func ToolNames(t testing.TB, list *mcp.Message) []string {
	t.Helper()
	var result struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(list.Result, &result); err != nil {
		t.Fatalf("mcptest: decode tools/list: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// ToolResult decodes a tools/call response. A JSON-RPC level error fails the
// test; a tool-level error (isError) is returned for the caller to assert on.
func ToolResult(t testing.TB, call *mcp.Message) mcp.CallToolResult {
	t.Helper()
	if call.Error != nil {
		t.Fatalf("mcptest: tools/call returned a JSON-RPC error: %+v", call.Error)
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(call.Result, &result); err != nil {
		t.Fatalf("mcptest: decode tools/call result: %v", err)
	}
	return result
}

// Text concatenates the text content of a tool result.
func Text(result mcp.CallToolResult) string {
	out := ""
	for _, c := range result.Content {
		if c.Type == "text" {
			out += c.Text
		}
	}
	return out
}

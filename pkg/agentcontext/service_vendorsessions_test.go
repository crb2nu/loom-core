package agentcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/vendorsessions"
)

func TestHandleVendorSessionTailReturnsNewestParsedLines(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	root := t.TempDir()
	id := "11111111-aaaa-bbbb-cccc-000000000001"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var records []string
	for i := 1; i <= 6; i++ {
		records = append(records, fmt.Sprintf(`{"type":"user","timestamp":"2026-08-08T00:00:0%dZ","message":{"role":"user","content":"turn %d"}}`, i, i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &VendorSessionsSvc{store: vendorsessions.Store{ClaudeRoot: root, CodexRoot: t.TempDir()}}
	res, err := svc.HandleVendorSessionTail(context.Background(), map[string]any{
		"vendor": "claude", "id": id, "lines": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", res.Content[0].Text)
	}
	var body struct {
		Lines []struct {
			Role, Timestamp, Text string
		} `json:"lines"`
		TotalLines int  `json:"total_lines"`
		Truncated  bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &body); err != nil {
		t.Fatalf("decode %q: %v", res.Content[0].Text, err)
	}
	if len(body.Lines) != 2 || body.Lines[0].Text != "turn 5" || body.Lines[1].Text != "turn 6" {
		t.Fatalf("lines = %+v", body.Lines)
	}
	if body.Lines[0].Role != "user" || body.Lines[0].Timestamp != "2026-08-08T00:00:05Z" || body.TotalLines != 6 || !body.Truncated {
		t.Fatalf("body = %+v", body)
	}
}

func TestHandleVendorSessionTailMissingSessionIsError(t *testing.T) {
	svc := &VendorSessionsSvc{store: vendorsessions.Store{ClaudeRoot: t.TempDir(), CodexRoot: t.TempDir()}}
	res, err := svc.HandleVendorSessionTail(context.Background(), map[string]any{"vendor": "claude", "id": "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("missing session should return a tool error")
	}
}

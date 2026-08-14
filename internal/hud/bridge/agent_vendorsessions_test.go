package bridge

import (
	"encoding/json"
	"testing"
)

func TestVendorSessionTailCallsBoundedTool(t *testing.T) {
	var got map[string]any
	b := NewAgentBridge(&stubCaller{callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
		if name != "agent_context__agent_vendor_session_tail" {
			t.Fatalf("tool = %q", name)
		}
		got = args
		return mcpTextResult(`"{\"lines\":[{\"role\":\"assistant\",\"timestamp\":\"2026-08-08T00:00:00Z\",\"text\":\"done\"}],\"total_lines\":9,\"truncated\":true,\"degraded\":false}"`), nil
	}})
	result, err := b.VendorSessionTail(" claude ", " abc ", 900)
	if err != nil {
		t.Fatal(err)
	}
	if got["vendor"] != "claude" || got["id"] != "abc" || got["lines"] != 500 {
		t.Fatalf("args=%v", got)
	}
	if len(result.Lines) != 1 || result.Lines[0].Text != "done" || result.TotalLines != 9 || !result.Truncated {
		t.Fatalf("result=%+v", result)
	}
}

package bridge

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestPatternInstancesEnvelopeAndForwarding(t *testing.T) {
	var gotID any
	b := NewAgentBridge(&stubCaller{callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
		if name != "agent_context__agent_pattern_get" {
			t.Fatalf("name=%s", name)
		}
		gotID = args["pattern_id"]
		return toolPayload(t, map[string]any{"ok": true, "pattern": map[string]any{"id": "pattern-go", "instances": []any{
			map[string]any{"stamped_at": "2026-08-08T12:00:00Z", "plan_id": "plan-1", "target_project": "services/a", "run_id": "run-1", "run_status": "succeeded", "mr_ref": "!42", "mr_status": "merged"},
			map[string]any{"stamped_at": "2026-08-09T12:00:00Z", "plan_id": "plan-2", "target_project": "services/b"},
		}}}), nil
	}})
	instances, err := b.PatternInstances(" pattern-go ")
	if err != nil {
		t.Fatal(err)
	}
	if gotID != "pattern-go" || len(instances) != 2 || instances[0].MRStatus != "merged" || instances[1].RunID != "" {
		t.Fatalf("id=%v instances=%#v", gotID, instances)
	}
}

func TestPatternInstancesEmptyIsNonNil(t *testing.T) {
	b := NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) {
		return toolPayload(t, map[string]any{"pattern": map[string]any{"id": "p"}}), nil
	}})
	instances, err := b.PatternInstances("p")
	if err != nil || instances == nil || len(instances) != 0 {
		t.Fatalf("instances=%#v err=%v", instances, err)
	}
}

func TestPatternInstancesPropagatesToolError(t *testing.T) {
	b := NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("offline") }})
	if _, err := b.PatternInstances("p"); err == nil {
		t.Fatal("expected tool error")
	}
}

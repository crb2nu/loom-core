package agentcontext

import (
	"testing"
)

// TestParseWorkflowSteps_MapReduceFields covers the define-path parsing of
// map_reduce steps. These fields were previously dropped by the hand
// parser, so map_reduce definitions registered via the public API failed
// at execution time with "map_step_template is required".
func TestParseWorkflowSteps_MapReduceFields(t *testing.T) {
	steps, err := parseWorkflowSteps([]any{
		map[string]any{
			"id":        "map_phase",
			"step_type": "map_reduce",
			// JSON numbers decode to float64; YAML ints stay int. Both
			// must parse.
			"map_input_key": "partitions",
			"map_step_template": map[string]any{
				"id":          "partition_search",
				"step_type":   "tool",
				"tool_name":   "codebase_search",
				"server_name": "codebase-memory",
				"tool_args":   map[string]any{"query": "${item}"},
			},
			"reduce_tool_name":   "agent_context_summarize",
			"reduce_server_name": "agent-context",
			"reduce_tool_args":   map[string]any{"session_id": "${init.session_id}"},
			"max_concurrency":    float64(4),
		},
	})
	if err != nil {
		t.Fatalf("parseWorkflowSteps: %v", err)
	}
	s := steps[0]
	if s.StepType != StepTypeMapReduce {
		t.Errorf("step_type: got %v", s.StepType)
	}
	if s.MapInputKey != "partitions" {
		t.Errorf("map_input_key: got %q", s.MapInputKey)
	}
	if s.MapStepTemplate == nil {
		t.Fatal("map_step_template not parsed")
	}
	if s.MapStepTemplate.ToolName != "codebase_search" {
		t.Errorf("template tool_name: got %q", s.MapStepTemplate.ToolName)
	}
	if s.ReduceToolName != "agent_context_summarize" || s.ReduceServerName != "agent-context" {
		t.Errorf("reduce tool: got %q/%q", s.ReduceToolName, s.ReduceServerName)
	}
	if s.ReduceToolArgs == nil || s.ReduceToolArgs["session_id"] != "${init.session_id}" {
		t.Errorf("reduce_tool_args: got %v", s.ReduceToolArgs)
	}
	if s.MaxConcurrency != 4 {
		t.Errorf("max_concurrency: got %d", s.MaxConcurrency)
	}
}

func TestParseWorkflowSteps_ParallelStepsRecursion(t *testing.T) {
	steps, err := parseWorkflowSteps([]any{
		map[string]any{
			"id":        "fanout",
			"step_type": "parallel",
			"parallel_steps": []any{
				map[string]any{"id": "a", "step_type": "tool", "tool_name": "t1"},
				map[string]any{"id": "b", "step_type": "tool", "tool_name": "t2"},
			},
		},
	})
	if err != nil {
		t.Fatalf("parseWorkflowSteps: %v", err)
	}
	s := steps[0]
	if s.StepType != StepTypeParallel {
		t.Errorf("step_type: got %v", s.StepType)
	}
	if len(s.ParallelSteps) != 2 {
		t.Fatalf("parallel_steps: got %d children", len(s.ParallelSteps))
	}
	if s.ParallelSteps[0].ToolName != "t1" || s.ParallelSteps[1].ToolName != "t2" {
		t.Errorf("children: %+v", s.ParallelSteps)
	}
}

func TestParseWorkflowSteps_ApprovalTimeout(t *testing.T) {
	steps, err := parseWorkflowSteps([]any{
		map[string]any{
			"id":                       "gate",
			"step_type":                "approval",
			"approval_message":         "review",
			"approval_timeout_seconds": float64(120),
		},
	})
	if err != nil {
		t.Fatalf("parseWorkflowSteps: %v", err)
	}
	if steps[0].ApprovalTimeoutSeconds != 120 {
		t.Errorf("approval_timeout_seconds: got %d want 120", steps[0].ApprovalTimeoutSeconds)
	}
}

func TestParseWorkflowSteps_DefaultsPreserved(t *testing.T) {
	steps, err := parseWorkflowSteps([]any{
		map[string]any{"tool_name": "t1"},
	})
	if err != nil {
		t.Fatalf("parseWorkflowSteps: %v", err)
	}
	if steps[0].ID != "step-1" {
		t.Errorf("default id: got %q", steps[0].ID)
	}
	if steps[0].StepType != StepTypeTool {
		t.Errorf("default step_type: got %v", steps[0].StepType)
	}
}

func TestParseWorkflowSteps_NonObjectStepErrors(t *testing.T) {
	if _, err := parseWorkflowSteps([]any{"not-a-step"}); err == nil {
		t.Fatal("expected error for non-object step")
	}
}

// TestResolveAny_ArrayRecursion guards the []any branch: tool_args that
// nest templates inside arrays (e.g. agent_context_add entries) must
// resolve, not pass through verbatim.
func TestResolveAny_ArrayRecursion(t *testing.T) {
	input := map[string]any{"query": "how do retries work"}
	context := map[string]any{
		"synthesize": map[string]any{"summary": "retries use backoff"},
	}
	args := map[string]any{
		"entries": []any{
			map[string]any{
				"entry_type": "finding",
				"title":      "${input.query}",
				"content":    "${synthesize.summary}",
			},
		},
	}
	resolved := resolveVariables(args, input, context)
	entries, ok := resolved["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries shape: %v", resolved["entries"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("entry shape: %T", entries[0])
	}
	if entry["title"] != "how do retries work" {
		t.Errorf("title not resolved: %v", entry["title"])
	}
	if entry["content"] != "retries use backoff" {
		t.Errorf("content not resolved: %v", entry["content"])
	}
}

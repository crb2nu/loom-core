package main

import (
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/trace/noop"
)

func testServer() (*mcp.Server, []mcp.Tool) {
	server := mcp.NewServer("test", "test")
	tracer := noop.NewTracerProvider().Tracer("test")
	registerTools(server, nil, tracer)
	return server, server.Tools()
}

func toolByName(tools []mcp.Tool, name string) *mcp.Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func TestRegisterTools_RegistersCoreAgentToolFamilies(t *testing.T) {
	t.Parallel()

	_, tools := testServer()
	if len(tools) < 55 {
		t.Fatalf("tool count = %d, want >= 55", len(tools))
	}

	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Fatalf("duplicate tool registered: %s", tool.Name)
		}
		seen[tool.Name] = true
	}

	expected := []string{
		"agent_session_start",
		"agent_session_end",
		"agent_context_add",
		"agent_recall",
		"agent_task_add",
		"agent_task_update",
		"agent_presence_register",
		"agent_presence_heartbeat",
		"agent_memory_policy_get",
		"agent_workflow_define",
		"agent_workflow_start",
		"agent_file_claim_acquire",
		"agent_worktree_allocate",
		"agent_handoff_create",
		"agent_message_send",
		"agent_message_inbox",
		"agent_vendor_session_list",
		"agent_vendor_session_search",
		"agent_vendor_session_tail",
		"agent_embedding_backfill",
	}

	for _, name := range expected {
		if !seen[name] {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

func TestSessionStartSchema_HasRequiredFields(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_session_start")
	if tool == nil {
		t.Fatal("agent_session_start not found")
	}

	if tool.Description == "" {
		t.Error("expected non-empty description")
	}

	props := tool.InputSchema.Properties
	for _, field := range []string{"agent_id", "namespace", "description"} {
		if _, ok := props[field]; !ok {
			t.Errorf("expected property %q in schema", field)
		}
	}
}

func TestSessionEndSchema_RequiresSessionID(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_session_end")
	if tool == nil {
		t.Fatal("agent_session_end not found")
	}

	if len(tool.InputSchema.Required) == 0 {
		t.Error("expected session_id to be required")
	}

	found := false
	for _, r := range tool.InputSchema.Required {
		if r == "session_id" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'session_id' in required fields, got %v", tool.InputSchema.Required)
	}

	if _, ok := tool.InputSchema.Properties["summary_async"]; !ok {
		t.Error("expected summary_async property in session end schema")
	}
	if _, ok := tool.InputSchema.Properties["post_session_retro"]; !ok {
		t.Error("expected post_session_retro property in session end schema")
	}
}

func TestContextAddSchema_HasEntries(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_context_add")
	if tool == nil {
		t.Fatal("agent_context_add not found")
	}

	props := tool.InputSchema.Properties
	if _, ok := props["session_id"]; !ok {
		t.Error("expected session_id property")
	}
	if _, ok := props["entries"]; !ok {
		t.Error("expected entries property")
	}
}

func TestTaskAddSchema_HasTasksArray(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_task_add")
	if tool == nil {
		t.Fatal("agent_task_add not found")
	}

	props := tool.InputSchema.Properties
	if _, ok := props["session_id"]; !ok {
		t.Error("expected session_id property")
	}
	if _, ok := props["tasks"]; !ok {
		t.Error("expected tasks property")
	}
}

func TestTaskUpdateSchema_HasStatusField(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_task_update")
	if tool == nil {
		t.Fatal("agent_task_update not found")
	}

	props := tool.InputSchema.Properties
	if _, ok := props["task_id"]; !ok {
		t.Error("expected task_id property")
	}
	if _, ok := props["status"]; !ok {
		t.Error("expected status property")
	}
}

func TestMemoryAddSchema_HasItems(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_memory_add")
	if tool == nil {
		t.Fatal("agent_memory_add not found")
	}

	props := tool.InputSchema.Properties
	if _, ok := props["items"]; !ok {
		t.Error("expected items property")
	}
}

func TestRemovedMemoryLifecycleToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{
		"agent_memory_compress",
		"agent_memory_merge",
		"agent_memory_policy_set",
	}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed (SIMP-2)", name)
		}
	}

	// agent_memory_policy_get should still be present (read-only introspection)
	if tool := toolByName(tools, "agent_memory_policy_get"); tool == nil {
		t.Error("agent_memory_policy_get should be retained as read-only")
	}

	// promote/demote re-registered for the HUD memory panel bridge.
	for _, name := range []string{"agent_memory_promote", "agent_memory_demote"} {
		if tool := toolByName(tools, name); tool == nil {
			t.Errorf("tool %q should be registered (HUD bridge dependency)", name)
		}
	}
}

func TestRemovedCompactionToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{
		"agent_compaction_trigger",
		"agent_reconcile_trigger",
	}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed (SIMP-6)", name)
		}
	}

	// agent_compaction_status re-registered for the HUD compaction tile bridge.
	if tool := toolByName(tools, "agent_compaction_status"); tool == nil {
		t.Error("agent_compaction_status should be registered (HUD bridge dependency)")
	}
}

func TestRemovedTemplateToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{"agent_template_create", "agent_template_list"}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed (SIMP-7)", name)
		}
	}
}

func TestRemovedLowUtilityContextToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{
		"agent_context_get",
		"agent_context_delete",
		"agent_context_share",
		"agent_context_query_shared",
		"agent_context_link_codebase",
		"agent_context_stats",
	}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed (SIMP-8)", name)
		}
	}

	// Core context tools should still be present
	retained := []string{
		"agent_context_add", "agent_context_search",
		"agent_context_summarize", "agent_recall",
	}
	for _, name := range retained {
		if tool := toolByName(tools, name); tool == nil {
			t.Errorf("core context tool %q should still be registered", name)
		}
	}
}

func TestRemovedMemoryExportImportToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{"agent_memory_export", "agent_memory_import"}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed (SIMP-3)", name)
		}
	}
}

func TestGraphToolsRegistered(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	// Core graph tools plus the path/reasoning tools re-registered for the
	// HUD bridge (graph path + reasoning chain routes).
	retained := []string{
		"agent_entity_add", "agent_entity_get", "agent_entity_find",
		"agent_entity_delete", "agent_relation_add", "agent_relation_get",
		"agent_relation_delete", "agent_graph_query", "agent_graph_stats",
		"agent_graph_find_path", "agent_reasoning_chain_add",
		"agent_reasoning_chain_get", "agent_reasoning_chain_list",
	}
	for _, name := range retained {
		if tool := toolByName(tools, name); tool == nil {
			t.Errorf("graph tool %q should be registered", name)
		}
	}
}

func TestUnifiedRecallSchema_HasScopeAndQuery(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_recall")
	if tool == nil {
		t.Fatal("agent_recall not found")
	}

	if tool.Description == "" {
		t.Error("expected non-empty description")
	}

	props := tool.InputSchema.Properties
	for _, field := range []string{"query", "scope", "agent_id", "token_budget", "file_context", "memory_tiers"} {
		if _, ok := props[field]; !ok {
			t.Errorf("expected property %q in agent_recall schema", field)
		}
	}

	if len(tool.InputSchema.Required) == 0 {
		t.Error("expected query to be required")
	}
	found := false
	for _, r := range tool.InputSchema.Required {
		if r == "query" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'query' in required fields")
	}
}

func TestRemovedDeprecatedRecallAliasesAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	// These three were [Deprecated:] aliases that forwarded to the unified
	// recall internals. agent_recall is the only recall entrypoint now.
	removed := []string{"agent_context_recall", "agent_context_recall_enhanced", "agent_memory_recall"}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("deprecated recall alias %q should have been removed", name)
		}
	}
	if toolByName(tools, "agent_recall") == nil {
		t.Error("agent_recall must remain as the unified recall entrypoint")
	}
}

func TestRemovedAnnotationToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	// Superseded by agent_context_add with entry_type="annotation" (the shape
	// internal/hud/bridge already writes) plus agent_context_search to read.
	removed := []string{"agent_code_annotate", "agent_code_annotations_get"}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed", name)
		}
	}
}

func TestRemovedRecipeAliasesAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	// agent_recipe_* forwarded verbatim to agent_engram_* (tier=1).
	removed := []string{"agent_recipe_add", "agent_recipe_recall", "agent_recipe_list"}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("recipe alias %q should have been removed", name)
		}
	}
	for _, name := range []string{"agent_engram_add", "agent_engram_recall", "agent_engram_list"} {
		if toolByName(tools, name) == nil {
			t.Errorf("engram tool %q must remain (recipes forward here)", name)
		}
	}
}

func TestRemovedDeltaAndWorktreeTriggerToolsAreGone(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	removed := []string{
		// Delta/cursor primitive: never acquired a consumer.
		"agent_context_recall_since",
		// Manual triggers for the always-on background WorktreeReconciler.
		"agent_worktree_cleanup",
		"agent_worktree_reconcile",
		// Subsumed by agent_worktree_list(include_git_status=true).
		"agent_worktree_status",
	}
	for _, name := range removed {
		if tool := toolByName(tools, name); tool != nil {
			t.Errorf("tool %q should have been removed", name)
		}
	}
	for _, name := range []string{"agent_worktree_allocate", "agent_worktree_release", "agent_worktree_list"} {
		if toolByName(tools, name) == nil {
			t.Errorf("core worktree tool %q should still be registered", name)
		}
	}
}

func TestContextAddSchema_SupportsAnnotations(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	tool := toolByName(tools, "agent_context_add")
	if tool == nil {
		t.Fatal("agent_context_add not found")
	}

	entries, ok := tool.InputSchema.Properties["entries"].(map[string]any)
	if !ok {
		t.Fatal("entries property not found or wrong type")
	}
	items, ok := entries["items"].(map[string]any)
	if !ok {
		t.Fatal("entries.items not found or wrong type")
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("entries.items.properties not found or wrong type")
	}

	for _, field := range []string{"annotation_type", "symbol"} {
		if _, ok := props[field]; !ok {
			t.Errorf("expected property %q in context entry schema for annotation support", field)
		}
	}

	entryType, ok := props["entry_type"].(map[string]any)
	if !ok {
		t.Fatal("entry_type property not found")
	}
	enumVals, ok := entryType["enum"].([]string)
	if !ok {
		t.Fatal("entry_type enum not found")
	}
	found := false
	for _, v := range enumVals {
		if v == "annotation" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'annotation' in entry_type enum")
	}
}
func TestAllToolsHaveDescriptions(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
	}
}

func TestAllToolsHaveObjectSchema(t *testing.T) {
	t.Parallel()
	_, tools := testServer()

	for _, tool := range tools {
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q has schema type %q, expected 'object'", tool.Name, tool.InputSchema.Type)
		}
	}
}

package main

import (
	"fmt"
	"strings"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// iccWorkbenchTools returns the ICC/PM tool surface used by the icc-*
// workflow skills (a representative subset of the live servers).
func iccWorkbenchTools() []mcp.Tool {
	names := []string{
		"icc__icc_project_list",
		"icc__icc_project_brief",
		"icc__icc_project_status",
		"icc__icc_project_changes",
		"icc__icc_project_blocked",
		"icc__icc_needs_attention",
		"icc__icc_inbox_summary",
		"icc__icc_search",
		"icc__icc_decision_list",
		"icc__icc_decision_create",
		"icc__icc_risk_list",
		"icc__icc_risk_create",
		"icc__icc_milestone_list",
		"icc__icc_code_ref_list",
		"icc__icc_session_link_list",
		"icc__icc_action_item_create",
		"icc__icc_artifact_update",
		"icc__icc_artifact_reclassify",
		"icc__icc_quick_capture",
		"icc__icc_draft_status_compose",
		"icc__icc_workstream_list",
		"icc__icc_project_kanban",
		"icc-capture__icc_write_capture",
		"icc-capture__icc_promote_to_artifact",
		"icc-capture__icc_demote_artifact",
		"icc-capture__icc_archive_raw",
		"icc-capture__icc_unarchive_raw",
		"icc-capture__icc_lint_notes",
		"icc-capture__icc_format_slack_paste",
		"icc-capture__icc_format_meeting_notes",
		"icc-capture__icc_format_email_extract",
		"icc-capture__icc_format_standup",
		"pm__pm_project_status",
		"pm__pm_risk_list",
		"pm__pm_risk_create",
		"pm__pm_risk_update",
		"pm__pm_risk_link",
	}
	tools := make([]mcp.Tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, mcp.Tool{Name: n})
	}
	return tools
}

// corePatternTools materializes one tool per fully-qualified pattern in
// coreRequiredPatterns, mirroring a live daemon where every priority
// pattern matches. This makes the profile-cap math realistic: antigravity's
// 100-tool ceiling must fill from the head of the priority list before the
// tail blocks (pattern/engram, ICC/PM) are reached.
func corePatternTools() []mcp.Tool {
	tools := make([]mcp.Tool, 0, len(coreRequiredPatterns))
	for _, p := range coreRequiredPatterns {
		if strings.Contains(p, "__") {
			tools = append(tools, mcp.Tool{Name: p})
		}
	}
	return tools
}

func iccBlockEntries() []string {
	var entries []string
	for _, p := range coreRequiredPatterns {
		server := serverFromToolName(p)
		if server == "icc" || server == "icc-capture" || server == "pm" {
			entries = append(entries, p)
		}
	}
	return entries
}

func TestFilterProxyTools_AntigravityCoreDefaults(t *testing.T) {
	tools := corePatternTools()
	for i := 0; i < 130; i++ {
		tools = append(tools, mcp.Tool{Name: fmt.Sprintf("misc__tool_%03d", i)})
	}

	filtered := filterProxyTools(tools, "antigravity", "", 0)
	if len(filtered) != proxyToolLimitAntigravity {
		t.Fatalf("filtered tools = %d, want %d", len(filtered), proxyToolLimitAntigravity)
	}

	got := make(map[string]struct{}, len(filtered))
	for _, tool := range filtered {
		got[tool.Name] = struct{}{}
	}

	for _, want := range []string{
		"git__git_status",
		"gitlab__pipeline_summary",
		"codebase_memory__codebase_search",
		"quality__quality_check",
		"agent_context__agent_session_start",
		"agent_context__agent_session_list",
		"agent_context__agent_recall",
		"agent_context__agent_handoff_create",
		"agent_context__agent_handoff_accept",
		"agent_context__agent_handoff_inbox",
		"agent_context__agent_worktree_allocate",
		"agent_context__agent_plan_get",
		"agent_context__agent_plan_slice_get",
		"k8s_apps_k3s__k8s_getPods",
		// In this every-pattern-matches fixture the 100-tool ceiling cuts
		// mid-flux; prometheus/loki and later servers only fit live when
		// optional servers (quality, context7) are absent.
		"longhorn_k3s__k8s_getPods",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("expected core tool %q in filtered set", want)
		}
	}

	// The ICC/PM block sits past antigravity's ceiling by design: enabling
	// the icc servers must not displace anything in the antigravity set.
	// Antigravity ICC sessions opt into the icc-core profile instead.
	for _, name := range iccBlockEntries() {
		if _, ok := got[name]; ok {
			t.Fatalf("ICC tool %q must not appear in the antigravity-core set", name)
		}
	}
}

func TestFilterProxyTools_LLMCoreDefaults(t *testing.T) {
	tools := corePatternTools()
	for i := 0; i < 190; i++ {
		tools = append(tools, mcp.Tool{Name: fmt.Sprintf("misc__tool_%03d", i)})
	}

	filtered := filterProxyTools(tools, "codex", "", 0)
	if len(filtered) != proxyToolLimitLLM {
		t.Fatalf("filtered tools = %d, want %d", len(filtered), proxyToolLimitLLM)
	}

	got := make(map[string]struct{}, len(filtered))
	for _, tool := range filtered {
		got[tool.Name] = struct{}{}
	}
	for _, want := range []string{
		"git__git_status",
		"git__git_diff",
		"gitlab__pipeline_summary",
		"codebase_memory__codebase_search",
		"quality__quality_check",
		"agent_context__agent_session_start",
		"agent_context__agent_recall",
		"agent_context__agent_context_add",
		"agent_context__agent_task_add",
		"agent_context__agent_session_end",
		"agent_context__agent_plan_get",
		"agent_context__agent_plan_slice_update",
		"context7__resolve-library-id",
		"context7__get-library-docs",
		"tavily__search",
		"k8s_apps_k3s__k8s_getPods",
		"prometheus__query",
		"loki__loki_query",
		// BrowserKit-backed screenshot skills must be callable from profile-limited clients.
		"browserkit__screenshot",
		// Pattern Loom + engram tools must reach the Mills vendors (codex/gemini).
		"agent_context__agent_pattern_list",
		"agent_context__agent_pattern_stamp",
		"agent_context__agent_pattern_record_instance",
		"agent_context__agent_engram_recall",
		// Cross-vendor session bridge must reach both claude-code and codex.
		"agent_context__agent_message_send",
		"agent_context__agent_message_inbox",
		"agent_context__agent_vendor_session_list",
		"agent_context__agent_vendor_session_search",
		// Branch-MR awareness (mrwatch M2) must reach the MR-shepherding vendors.
		"agent_context__agent_mr_status",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("expected llm core tool %q in filtered set", want)
		}
	}

	// The full ICC/PM block must reach llm-core so the icc-* workflow
	// skills are functional on codex/claude — the 2026-07-14 audit found
	// the 140 cap exhausted before tail-fill could reach these servers.
	for _, name := range iccBlockEntries() {
		if _, ok := got[name]; !ok {
			t.Fatalf("expected ICC tool %q in llm-core set", name)
		}
	}
}

func TestFilterProxyTools_ICCCoreProfile(t *testing.T) {
	tools := corePatternTools()
	tools = append(tools, iccWorkbenchTools()...)
	for i := 0; i < 60; i++ {
		tools = append(tools, mcp.Tool{Name: fmt.Sprintf("misc__tool_%03d", i)})
	}

	filtered := filterProxyTools(tools, "", proxyToolProfileICCCore, 0)
	if len(filtered) > proxyToolLimitICC {
		t.Fatalf("filtered tools = %d, want <= %d", len(filtered), proxyToolLimitICC)
	}

	got := make(map[string]struct{}, len(filtered))
	for _, tool := range filtered {
		got[tool.Name] = struct{}{}
	}

	// Slim developer core survives.
	for _, want := range []string{
		"git__git_status",
		"git__git_commit",
		"gitlab__create_merge_request",
		"codebase_memory__codebase_search",
		"agent_context__agent_session_start",
		"agent_context__agent_task_add",
		"tavily__search",
		"time__get_current_time",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("expected slim core tool %q in icc-core set", want)
		}
	}

	// The entire ICC workbench surface is present.
	for _, tool := range iccWorkbenchTools() {
		if _, ok := got[tool.Name]; !ok {
			t.Fatalf("expected ICC workbench tool %q in icc-core set", tool.Name)
		}
	}
}

func TestResolveProxyToolFilter_InferLLMCoreFromAgentHint(t *testing.T) {
	for _, agentHint := range []string{"codex", "claude", "claude-code"} {
		profile, limit := resolveProxyToolFilter(agentHint, "", 0)
		if profile != proxyToolProfileLLMCore {
			t.Fatalf("agentHint %q resolved profile = %q, want %q", agentHint, profile, proxyToolProfileLLMCore)
		}
		if limit != proxyToolLimitLLM {
			t.Fatalf("agentHint %q resolved limit = %d, want %d", agentHint, limit, proxyToolLimitLLM)
		}
	}
}

func TestResolveProxyToolFilter_ICCCoreDefaultLimit(t *testing.T) {
	profile, limit := resolveProxyToolFilter("", proxyToolProfileICCCore, 0)
	if profile != proxyToolProfileICCCore {
		t.Fatalf("resolved profile = %q, want %q", profile, proxyToolProfileICCCore)
	}
	if limit != proxyToolLimitICC {
		t.Fatalf("resolved limit = %d, want %d", limit, proxyToolLimitICC)
	}
}

func TestFilterProxyTools_MaxToolsOnly(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "git__git_status"},
		{Name: "git__git_diff"},
		{Name: "git__git_log"},
		{Name: "git__git_show"},
	}

	filtered := filterProxyTools(tools, "", "", 2)
	if len(filtered) != 2 {
		t.Fatalf("filtered length = %d, want 2", len(filtered))
	}
	if filtered[0].Name != "git__git_status" || filtered[1].Name != "git__git_diff" {
		t.Fatalf("unexpected filtered tools: %#v", filtered)
	}
}

package main

import (
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

const (
	proxyToolProfileAntigravityCore = "antigravity-core"
	proxyToolProfileLLMCore         = "llm-core"
	proxyToolProfileICCCore         = "icc-core"
	proxyToolLimitAntigravity       = 100
	// Raised 140 → 160 (2026-07-14) to fit the ICC/PM block at the tail of
	// the core priority list without displacing existing selections. The
	// LLM-core vendors (codex/claude/kilocode) have no hard tool ceiling;
	// 140 was a context-economy choice, and the audit showed the priority
	// list + serverOrder quotas exhausted it before tail-fill could reach
	// icc/icc-capture/pm, leaving the ICC workflow skills non-functional.
	// Raised 160 → 164 (2026-07-15) for the cross-vendor session bridge
	// block (agent_message_send/inbox, agent_vendor_session_list/search):
	// the priority list already sat at ≈156/160 with icc/pm enabled, so
	// without the bump the new block would displace the ICC/PM tail.
	// Raised 164 → 165 (2026-07-19) for the branch-MR awareness tool
	// (agent_mr_status, mrwatch M2): a single tail addition, so without the
	// bump it would displace the last ICC/PM entry (silent no-op).
	// Raised 165 → 166 (2026-07-25) for agent_plan_slice_add. The Mills
	// operator prompt (cmd/loom-mills-operator) instructs slice authoring, but
	// the tool was never in the priority list, so profile-limited workers
	// silently could not call it. SILENT-DISPLACEMENT HAZARD: adding a pattern
	// to coreRequiredPatterns without bumping this cap does not error — it
	// evicts the last entry of the ICC/PM tail instead.
	// Raised 166 → 167 (2026-07-27) for browserkit__screenshot so the
	// BrowserKit screenshot skill is usable from profile-limited clients.
	proxyToolLimitLLM = 167
	// icc-core shares Antigravity's 100-tool platform ceiling so the
	// profile is usable on every profile-limited client.
	proxyToolLimitICC = 100
)

// filterProxyTools applies per-client tool shaping at the proxy boundary.
// This keeps global daemon tool caches unchanged while allowing platform-
// specific caps (like Antigravity's 100-tool ceiling).
func filterProxyTools(tools []mcp.Tool, agentHint, profile string, maxTools int) []mcp.Tool {
	if len(tools) == 0 {
		return tools
	}

	resolvedProfile, resolvedLimit := resolveProxyToolFilter(agentHint, profile, maxTools)
	switch resolvedProfile {
	case proxyToolProfileAntigravityCore, proxyToolProfileLLMCore:
		return selectCoreDeveloperTools(tools, resolvedLimit)
	case proxyToolProfileICCCore:
		return selectICCWorkbenchTools(tools, resolvedLimit)
	}

	if resolvedLimit > 0 && len(tools) > resolvedLimit {
		return append([]mcp.Tool(nil), tools[:resolvedLimit]...)
	}
	return tools
}

func resolveProxyToolFilter(agentHint, profile string, maxTools int) (string, int) {
	resolvedProfile := strings.ToLower(strings.TrimSpace(profile))
	if resolvedProfile == "" {
		switch normalized := strings.ToLower(strings.TrimSpace(agentHint)); normalized {
		case "antigravity":
			resolvedProfile = proxyToolProfileAntigravityCore
		case "codex", "claude", "claude-code":
			resolvedProfile = proxyToolProfileLLMCore
		}
	}
	if maxTools <= 0 {
		switch resolvedProfile {
		case proxyToolProfileAntigravityCore:
			maxTools = proxyToolLimitAntigravity
		case proxyToolProfileLLMCore:
			maxTools = proxyToolLimitLLM
		case proxyToolProfileICCCore:
			maxTools = proxyToolLimitICC
		}
	}
	return resolvedProfile, maxTools
}

// selectCoreDeveloperTools shapes the shared developer core used by the
// antigravity-core and llm-core profiles.
func selectCoreDeveloperTools(tools []mcp.Tool, limit int) []mcp.Tool {
	if limit <= 0 {
		limit = proxyToolLimitLLM
	}
	return selectProfileTools(tools, limit, coreRequiredPatterns, coreServerOrder, coreServerQuota)
}

// selectICCWorkbenchTools shapes the icc-core profile: a slim developer core
// plus the full ICC workbench surface (icc, icc-capture, pm). Sized to fit
// Antigravity's 100-tool ceiling so ICC-focused sessions work on every
// profile-limited client. Opt in with `loom proxy --tool-profile icc-core`.
func selectICCWorkbenchTools(tools []mcp.Tool, limit int) []mcp.Tool {
	if limit <= 0 {
		limit = proxyToolLimitICC
	}
	return selectProfileTools(tools, limit, iccCoreRequiredPatterns, iccCoreServerOrder, iccCoreServerQuota)
}

func selectProfileTools(tools []mcp.Tool, limit int, requiredPatterns, serverOrder []string, serverQuota map[string]int) []mcp.Tool {
	selected := make([]mcp.Tool, 0, min(limit, len(tools)))
	seen := make(map[string]struct{}, len(tools))

	addTool := func(tool mcp.Tool) bool {
		if len(selected) >= limit {
			return false
		}
		if _, ok := seen[tool.Name]; ok {
			return true
		}
		seen[tool.Name] = struct{}{}
		selected = append(selected, tool)
		return true
	}

	addByPattern := func(pattern string) bool {
		if strings.Contains(pattern, "__") {
			for _, tool := range tools {
				if tool.Name == pattern {
					return addTool(tool)
				}
			}
			return true
		}

		suffix := "__" + pattern
		for _, tool := range tools {
			if tool.Name == pattern || strings.HasSuffix(tool.Name, suffix) {
				return addTool(tool)
			}
		}
		return true
	}

	for _, pattern := range requiredPatterns {
		if !addByPattern(pattern) {
			return selected
		}
	}

	toolsByServer := make(map[string][]mcp.Tool)
	for _, tool := range tools {
		server := serverFromToolName(tool.Name)
		toolsByServer[server] = append(toolsByServer[server], tool)
	}

	for _, server := range serverOrder {
		quota := serverQuota[server]
		if quota == 0 {
			quota = 4
		}
		added := 0
		for _, tool := range toolsByServer[server] {
			if len(selected) >= limit {
				return selected
			}
			if _, ok := seen[tool.Name]; ok {
				continue
			}
			if !addTool(tool) {
				return selected
			}
			added++
			if added >= quota {
				break
			}
		}
	}

	for _, tool := range tools {
		if len(selected) >= limit {
			break
		}
		if _, ok := seen[tool.Name]; ok {
			continue
		}
		if !addTool(tool) {
			break
		}
	}

	return selected
}

var coreRequiredPatterns = []string{
	"git__git_status",
	"git__git_diff",
	"git__git_log",
	"git__git_show",
	"git__git_add",
	"git__git_commit",
	"git__git_checkout",
	"git__git_branch",
	"git__git_pull",
	"git__git_push",
	"git__git_stash",
	"git_worktree__git_worktree_list",
	"git_worktree__git_worktree_add",
	"git_worktree__git_worktree_remove",
	"github__get_repo",
	"github__list_issues",
	"github__get_issue",
	"github__list_prs",
	"github__get_pr",
	"github__get_file_contents",
	"github__search_code",
	"gitlab__get_project",
	"gitlab__list_projects",
	"gitlab__list_issues",
	"gitlab__list_merge_requests",
	"gitlab__list_pipelines",
	"gitlab__pipeline_summary",
	"gitlab__get_pipeline",
	"gitlab__list_pipeline_jobs",
	"gitlab__get_job_trace",
	"gitlab__poll_pipeline",
	"gitlab__create_merge_request",
	// merge_merge_request pairs with create_merge_request: the shipping
	// skills (feature-dev, bugfix, small-change-loop) instruct
	// merge_merge_request(auto_merge=true); without it profile-limited
	// clients can open MRs they cannot land.
	"gitlab__merge_merge_request",
	"gitlab__create_issue",
	"gitlab__update_issue",
	"codebase_memory__codebase_search",
	"codebase_memory__codebase_text_search",
	"codebase_memory__codebase_get_definition",
	"codebase_memory__codebase_get_context",
	"codebase_memory__codebase_get_references",
	"codebase_memory__codebase_find_callers",
	"codebase_memory__codebase_find_callees",
	"codebase_memory__codebase_call_graph",
	"codebase_memory__codebase_module_graph",
	"codebase_memory__codebase_stats",
	"quality__quality_check",
	"quality__quality_lint",
	"quality__quality_test",
	"quality__quality_security",
	"quality__quality_coverage",
	"agent_context__agent_session_start",
	"agent_context__agent_session_list",
	"agent_context__agent_recall",
	"agent_context__agent_context_add",
	"agent_context__agent_task_add",
	"agent_context__agent_task_update",
	"agent_context__agent_task_list",
	"agent_context__agent_handoff_create",
	"agent_context__agent_handoff_accept",
	"agent_context__agent_handoff_inbox",
	"agent_context__agent_worktree_allocate",
	// worktree_release pairs with worktree_allocate: workflow-skill
	// cleanup steps call it after merge.
	"agent_context__agent_worktree_release",
	"agent_context__agent_presence_register",
	"agent_context__agent_presence_heartbeat",
	"agent_context__agent_session_end",
	// Plan store: worktree-resilient, cross-vendor plan/slice resolution.
	// Without these in the core profile, profile-limited clients (codex,
	// antigravity) cannot resolve plans via agent_plan_get and silently
	// fall back to fuzzy context recall — see the 2026-06-24 kill-test.
	"agent_context__agent_plan_get",
	"agent_context__agent_plan_list",
	"agent_context__agent_plan_create",
	"agent_context__agent_plan_update",
	"agent_context__agent_plan_lifecycle_advance",
	"agent_context__agent_plan_slice_get",
	"agent_context__agent_plan_slice_list",
	"agent_context__agent_plan_slice_update",
	"agent_context__agent_plan_slice_claim",
	// slice_add completes the set: the Mills operator prompt tells workers to
	// author slices, and without this the instruction is a no-op for every
	// profile-limited client. Cap raised 165 → 166 alongside it.
	"agent_context__agent_plan_slice_add",
	"tavily__search",
	"tavily__search_news",
	"tavily__extract",
	"context7__resolve-library-id",
	"context7__get-library-docs",
	"time__get_current_time",
	"time__convert_timezone",
	"time__add_duration",
	"docker__docker_ps",
	"docker__docker_logs",
	"docker__docker_exec",
	"devbox__devbox_status",
	"devbox__devbox_exec",
	"devbox__devbox_exec_poll",
	// quality_gate is the structured fmt→lint→test entrypoint the
	// auto-quality-gate skill and workflow verification steps rely on.
	"devbox__devbox_quality_gate",
	"k8s_apps_k3s__k8s_getPods",
	"k8s_apps_k3s__k8s_logs",
	"k8s_apps_k3s__k8s_get",
	"k8s_apps_k3s__k8s_describe",
	"k8s_harvester_infra__k8s_getPods",
	"k8s_harvester_infra__k8s_logs",
	"k8s_harvester_infra__k8s_get",
	"longhorn_k3s__k8s_getPods",
	"longhorn_k3s__k8s_logs",
	"longhorn_k3s__k8s_get",
	"flux__flux_get_kustomizations",
	"flux__flux_get_sources",
	"flux__flux_reconcile",
	"prometheus__query",
	"prometheus__query_range",
	"loki__loki_query",
	"loki__loki_query_range",
	"grafana__grafana_search",
	"grafana__grafana_get_dashboard",
	"helm__helm_list",
	"helm__helm_status",
	"cloudflare__cf_list_zones",
	"cloudflare__cf_list_dns_records",
	"release__release_status",
	"release__release_validate",
	// BrowserKit is local-only, but the llm-core proxy filter still needs to
	// select its screenshot tool for Codex, Claude, and Kilo clients.
	"browserkit__screenshot",
	// Mills Pattern Loom + engram tech-tree. Placed at the TAIL on purpose:
	// the LLM-core profile (codex/gemini/claude — the vendors that run the
	// autonomous Mills council, squads, and stamp path) has headroom for
	// these, so A1 rails can cite an approved pattern, B1/stamp can fire,
	// and A2's green-stamp engram hook is reachable cross-vendor. The
	// tighter antigravity ceiling truncates before reaching them, so its
	// existing tool set is unchanged (antigravity is not a Mills vendor).
	"agent_context__agent_pattern_list",
	"agent_context__agent_pattern_get",
	"agent_context__agent_pattern_search",
	"agent_context__agent_pattern_add",
	"agent_context__agent_pattern_stamp",
	"agent_context__agent_pattern_promote",
	"agent_context__agent_pattern_record_instance",
	"agent_context__agent_engram_recall",
	"agent_context__agent_engram_list",
	"agent_context__agent_engram_verify",
	// Cross-vendor session bridge. Placed at the TAIL like the blocks
	// above: llm-core (codex/claude/gemini) reaches it — these tools are
	// how the vendors search each other's local CLI transcripts and send
	// each other direct messages — while antigravity's 100 cap truncates
	// first, leaving its set unchanged. Cap raised 160 → 164 alongside
	// this block; see proxyToolLimitLLM.
	"agent_context__agent_message_send",
	"agent_context__agent_message_inbox",
	"agent_context__agent_vendor_session_list",
	"agent_context__agent_vendor_session_search",
	// Branch-MR awareness (mrwatch M2). Placed at the TAIL like the blocks
	// above: llm-core (codex/claude/gemini — the vendors that open and shepherd
	// MRs) reaches it, so a session can ask agent_mr_status whether an MR it
	// pushed has stalled; antigravity's 100 cap truncates first, leaving its
	// set unchanged. Cap raised 164 → 165 alongside this single addition; see
	// proxyToolLimitLLM.
	"agent_context__agent_mr_status",
	// ICC workbench + PM block. Placed at the TAIL on purpose, like the
	// pattern/engram block above: llm-core's cap reaches it (the full
	// priority list fits within proxyToolLimitLLM, displacing only
	// serverOrder fill), while antigravity-core's 100 cap truncates before it, leaving
	// the antigravity tool set byte-for-byte unchanged. Antigravity ICC
	// sessions should use the dedicated icc-core profile instead. These
	// are the tools the icc-* workflow skills in skills-registry.yaml
	// instruct (daily-pm/weekly-status/triage/handoff-prep/capture-ops).
	// NOTE: icc__* patterns only match when the `icc` server is enabled in
	// catalog-state.yaml; unmatched patterns cost no slots.
	"icc__icc_project_list",
	"icc__icc_project_brief",
	"icc__icc_project_status",
	"icc__icc_project_changes",
	"icc__icc_project_blocked",
	"icc__icc_needs_attention",
	"icc__icc_inbox_summary",
	"icc__icc_search",
	"icc__icc_decision_list",
	"icc__icc_risk_list",
	"icc__icc_milestone_list",
	"icc__icc_code_ref_list",
	"icc__icc_session_link_list",
	"icc__icc_decision_create",
	"icc__icc_risk_create",
	"icc__icc_action_item_create",
	"icc__icc_artifact_update",
	"icc__icc_artifact_reclassify",
	"icc__icc_quick_capture",
	"icc__icc_draft_status_compose",
	"icc-capture__icc_write_capture",
	"icc-capture__icc_promote_to_artifact",
	"icc-capture__icc_demote_artifact",
	"icc-capture__icc_archive_raw",
	"icc-capture__icc_lint_notes",
	"icc-capture__icc_format_slack_paste",
	"icc-capture__icc_format_meeting_notes",
	"pm__pm_project_status",
	"pm__pm_risk_list",
	"pm__pm_risk_create",
	"pm__pm_risk_update",
	"pm__pm_risk_link",
}

var coreServerOrder = []string{
	"git", "git_worktree", "github", "gitlab", "codebase_memory", "quality",
	"agent_context", "devbox", "docker", "k8s_apps_k3s", "k8s_harvester_infra",
	"longhorn_k3s", "flux", "prometheus", "loki", "grafana", "helm",
	"cloudflare", "tavily", "context7", "time", "release",
}

var coreServerQuota = map[string]int{
	"git":                 12,
	"gitlab":              16,
	"codebase_memory":     12,
	"agent_context":       14,
	"k8s_apps_k3s":        8,
	"k8s_harvester_infra": 8,
	"longhorn_k3s":        8,
	"devbox":              6,
	"docker":              5,
	"flux":                6,
	"prometheus":          5,
	"loki":                5,
	"grafana":             5,
	"helm":                5,
	"cloudflare":          5,
}

// icc-core: slim developer basics up front, then the full ICC surface via
// server quotas (icc ≈55 tools, icc-capture ≈15, pm ≈6). ~28 basics + the
// workbench tools fill the 100 cap with the ICC surface intact; the pm
// tools the ICC skills lean on hardest are pinned in the priority list so
// the cap cannot squeeze them out.
var iccCoreRequiredPatterns = []string{
	"git__git_status",
	"git__git_diff",
	"git__git_log",
	"git__git_add",
	"git__git_commit",
	"git__git_checkout",
	"git__git_push",
	"gitlab__get_project",
	"gitlab__list_issues",
	"gitlab__create_issue",
	"gitlab__update_issue",
	"gitlab__list_merge_requests",
	"gitlab__create_merge_request",
	"gitlab__merge_merge_request",
	"pm__pm_project_status",
	"codebase_memory__codebase_search",
	"codebase_memory__codebase_text_search",
	"agent_context__agent_session_start",
	"agent_context__agent_session_end",
	"agent_context__agent_recall",
	"agent_context__agent_context_add",
	"agent_context__agent_task_add",
	"agent_context__agent_task_update",
	"agent_context__agent_task_list",
	"agent_context__agent_handoff_create",
	"tavily__search",
	"tavily__extract",
	"time__get_current_time",
}

var iccCoreServerOrder = []string{"icc", "icc-capture", "pm"}

var iccCoreServerQuota = map[string]int{
	"icc":         60,
	"icc-capture": 16,
	"pm":          6,
}

func serverFromToolName(name string) string {
	if idx := strings.Index(name, "__"); idx > 0 {
		return name[:idx]
	}
	return ""
}

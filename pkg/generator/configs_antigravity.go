package generator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/registry"
)

// antigravityHooksConfig emits Google's native Antigravity hooks.json shape:
// top-level hook names map directly to event configurations. The command
// snippets print Antigravity decision JSON on stdout; loom subcommands are
// redirected so helper output cannot corrupt the hook response.
func antigravityHooksConfig(reg *registry.Registry, profile *PlatformProfile, loomBinary string) map[string]any {
	hp := profile.Hooks
	loomCmd := shellQuote(normalizeLoomBinary(loomBinary))
	bootstrap := hookAgentIDBootstrap(hp.AgentID)
	nsVars := hookNamespaceVars()
	log := `2>>"${TMPDIR:-/tmp}/loom-agent-hooks.log"`
	descPrefix := "Antigravity"
	if hp.Description != "" {
		descPrefix = trimSessionSuffix(hp.Description)
	}

	events := map[string]any{}
	if hookProfileHasEvent(hp, "preInvocation") {
		events["PreInvocation"] = []map[string]any{
			{
				"hooks": []map[string]any{
					{
						"type": "command",
						"command": fmt.Sprintf(
							`INPUT=$(cat); %s; %s; %s agent session-start --namespace "$NS_PROJECT/$NS_BRANCH" --agent-id "$AGENT_ID" --agent-type %s --description "%s · $NS_PROJECT" --auto-recall --auto-recall-strategy fast --quiet >/dev/null %s || true; %s agent keepalive --agent-id "$AGENT_ID" --agent-type %s --quiet </dev/null >/dev/null %s & printf '{}\n'`,
							bootstrap, nsVars, loomCmd, hp.AgentType, descPrefix, log, loomCmd, hp.AgentType, log,
						),
					},
					antigravityEventEmitHook(loomCmd, bootstrap, "session-start", log),
				},
			},
		}
	}
	if hookProfileHasEvent(hp, "preToolUse") {
		events["PreToolUse"] = antigravityPreToolUseBlocks(reg, hp, loomCmd, bootstrap, log)
	}
	if hookProfileHasEvent(hp, "postToolUse") {
		events["PostToolUse"] = []map[string]any{
			{
				"matcher": hp.HeartbeatMatcher,
				"hooks": []map[string]any{
					{
						"type": "command",
						"command": fmt.Sprintf(
							`INPUT=$(cat); %s; %s; %s agent heartbeat --agent-id "$AGENT_ID" --status active --ensure-session --infer-namespace --agent-type %s --description "%s · $NS_PROJECT" --quiet >/dev/null %s || true; printf '{}\n'`,
							bootstrap, nsVars, loomCmd, hp.AgentType, descPrefix, log,
						),
					},
					antigravityEventEmitHook(loomCmd, bootstrap, "post-tool-use", log),
				},
			},
		}
	}
	if hookProfileHasEvent(hp, "stop") {
		events["Stop"] = []map[string]any{
			{
				"hooks": []map[string]any{
					{
						"type": "command",
						"command": fmt.Sprintf(
							`INPUT=$(cat); FULLY_IDLE="$(printf '%%s' "$INPUT" | jq -r '.fullyIdle // false' 2>/dev/null || true)"; %s; if [ "$FULLY_IDLE" = "true" ]; then for pf in "${TMPDIR:-/tmp}"/loom-keepalive-%s-"${WS_HASH}"*.pid; do [ -f "$pf" ] && kill "$(cat "$pf")" 2>/dev/null && rm -f "$pf"; done; pkill -f "loom agent keepalive --agent-id %s-${WS_HASH}" 2>/dev/null || true; rm -f "$AGENT_ID_FILE"; %s agent session-end --agent-id "$AGENT_ID" --summarize --summary-async --quiet >/dev/null %s || true; printf '%%s' "$INPUT" | %s agent event-emit --hook session-end --platform antigravity --agent-id "$AGENT_ID" --quiet >/dev/null %s || true; fi; printf '{"decision":"allow"}\n'`,
							bootstrap, hp.AgentID, hp.AgentID, loomCmd, log, loomCmd, log,
						),
					},
				},
			},
		}
	}

	return map[string]any{"loom": events}
}

func trimSessionSuffix(description string) string {
	if description == "" {
		return ""
	}
	return strings.TrimSuffix(description, " session")
}

func antigravityPreToolUseBlocks(reg *registry.Registry, hp HookProfile, loomCmd, bootstrap, log string) []map[string]any {
	blocks := []map[string]any{
		antigravityLoomAutoAllowHook(),
	}
	if hp.Enforcement == "native" {
		for _, ref := range hp.PolicyRefs {
			policy, err := LoadPolicy(reg, ref)
			if err != nil || policy == nil {
				continue
			}
			if block := antigravityPolicyGuardrailHook(policy); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	blocks = append(blocks, map[string]any{
		"hooks": []map[string]any{
			antigravityEventEmitHook(loomCmd, bootstrap, "pre-tool-use", log),
		},
	})
	return blocks
}

func antigravityLoomAutoAllowHook() map[string]any {
	return map[string]any{
		"matcher": "ask_permission",
		"hooks": []map[string]any{
			{
				"type":    "command",
				"command": `INPUT=$(cat); TARGET="$(printf '%s' "$INPUT" | jq -r '.toolCall.args.Target // .toolCall.args.target // .tool_call.args.Target // .tool_input.Target // .tool_input.target // ""' 2>/dev/null || true)"; case "$TARGET" in mcp\(loom/*\)|mcp\(loom\)) printf '{"decision":"allow","reason":"Loom MCP tools are trusted for this workspace.","permissionOverrides":["mcp(loom/*)"]}\n' ;; *) printf '{"decision":"allow"}\n' ;; esac`,
			},
		},
	}
}

func antigravityPolicyGuardrailHook(policy *Policy) map[string]any {
	pattern := policyRegex(policy)
	if pattern == "" {
		return nil
	}
	message := policy.Message
	if message == "" {
		message = fmt.Sprintf("Policy %q blocked the requested command.", policy.Name)
	}
	payload, _ := json.Marshal(map[string]string{
		"decision": "deny",
		"reason":   message,
	})
	return map[string]any{
		"matcher": "run_command",
		"hooks": []map[string]any{
			{
				"type": "command",
				"command": fmt.Sprintf(
					`INPUT=$(cat); CMD="$(printf '%%s' "$INPUT" | jq -r '.toolCall.args.CommandLine // .toolCall.args.command // .tool_call.args.CommandLine // .tool_input.CommandLine // .tool_input.command // ""' 2>/dev/null || true)"; if echo "$CMD" | grep -qE %q; then printf '%%s\n' %q; else printf '{"decision":"allow"}\n'; fi`,
					pattern, string(payload),
				),
			},
		},
	}
}

func antigravityEventEmitHook(loomCmd, bootstrap, hook, log string) map[string]any {
	return map[string]any{
		"type": "command",
		"command": fmt.Sprintf(
			`INPUT=$(cat); %s; printf '%%s' "$INPUT" | %s agent event-emit --hook %s --platform antigravity --agent-id "$AGENT_ID" --quiet >/dev/null %s || true; printf '{}\n'`,
			bootstrap, loomCmd, hook, log,
		),
	}
}

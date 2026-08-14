Re-registered the agent-context MCP tools the HUD bridge still calls
(`agent_graph_find_path`, `agent_reasoning_chain_add/get/list`,
`agent_memory_promote/demote`, `agent_compaction_status`) that the SIMP-2/4/6
slices had removed, fixing "unknown tool" failures on the graph path, reasoning
chain, memory promote/demote, and compaction status HUD/mobile endpoints. The
bridge now speaks the tools' actual wire format (chain_id/query payloads). The
orphaned session-template surface (`GET /api/templates`, bridge TemplateList,
frontend templates UI, deprecated TemplateSvc stub) was deleted outright, and
registry always_allow entries were updated to match the registered tool set.

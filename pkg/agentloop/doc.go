// Package agentloop is the reusable F4-tool-loop-as-prefix ReAct engine.
//
// It drives an append-only, mutability-ordered conversation against an
// OpenAI-compatible chat endpoint (the flexinfer proxy), pinning a session
// cache key so every turn lands on the replica holding the warm KV prefix.
// The whole design exists to make the prefix cache pay off: the system
// message plus the fixed tool set form an immutable prefix, and history grows
// by Append only — never reordered or rewritten — so each turn's wire payload
// is a block-aligned prefix extension of the previous turn's.
//
// This package is a faithful port of the flexinfer CLI engine
// (services/flexinfer/internal/agentloop), lifted into loom-core so the same
// shape can be exposed as an MCP tool (cmd/mcp-agent-loop). The two live in
// different Go modules, so the layout is mirrored rather than imported.
//
// The live signal it was built to produce — flat per-turn upstream latency
// while prompt_tokens grows — was validated end-to-end on 2026-06-01 against
// the gemma4 APC canary (flexinfer matrix row 195, PASS): a 22.5x prompt
// growth held upstream_ms tracking the per-round token delta, not the
// cumulative prompt.
package agentloop

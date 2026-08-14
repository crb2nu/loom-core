package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the rot guard for the agent-context tool surface.
//
// Three surfaces name agent_context tools and drift apart silently:
//
//  1. cmd/mcp-agent-context — the registrations (source of truth).
//  2. mcp/context/registry.yaml `always_allow` — feeds pkg/generator, which
//     writes the per-vendor auto-approval lists. Stale names here are dead
//     config; missing names mean an approval prompt on every call.
//  3. cmd/loom/proxy_tool_filter.go `coreRequiredPatterns` — the llm-core /
//     antigravity-core priority list. A pattern naming a tool the server does
//     not register is a silent no-op (see the 2026-06-24 plan-store kill-test).
//
// Every past drift in this area was invisible until something broke in
// production, so these assertions are deliberately exact rather than advisory.

// mutatingToolsDenylist is the explicit "registered but deliberately NOT
// auto-approved" set. Every registered tool must be in registry.yaml's
// always_allow OR here — adding a tool without classifying it fails the build.
//
// These either write shared cross-agent state or fan work out to other agents,
// so they should surface an approval prompt rather than run silently.
var mutatingToolsDenylist = map[string]string{
	"agent_embedding_backfill":      "rewrites degraded vectors in shared Qdrant collections",
	"agent_engram_add":              "writes a shared, proof-gated capsule to the engram tech tree",
	"agent_memory_demote":           "moves shared memory items down a tier; HUD bridge surface",
	"agent_memory_promote":          "moves shared memory items up a tier; HUD bridge surface",
	"agent_reasoning_chain_add":     "writes a shared reasoning chain; HUD bridge surface",
	"agent_pattern_add":             "writes to the shared Mills pattern catalog",
	"agent_pattern_promote":         "changes a pattern's catalog status",
	"agent_pattern_record_instance": "attributes a pattern instance to a repo/MR",
	"agent_pattern_stamp":           "green-stamps a pattern; feeds Mills autonomy gates",
	"agent_plan_create":             "creates a cross-vendor plan other agents will execute",
	"agent_plan_update":             "rewrites a plan other agents may already be executing",
	"agent_plan_lifecycle_advance":  "advances plan lifecycle state",
	"agent_plan_slice_add":          "adds executable work to a shared plan",
	"agent_plan_slice_claim":        "claims a slice away from other agents",
	"agent_plan_slice_update":       "mutates a slice another agent may hold",
	"agent_session_prune":           "bulk-deletes session records",
	"agent_task_dispatch":           "pushes work onto another agent's queue",
}

type registryFile struct {
	Servers []struct {
		Name   string `yaml:"name"`
		Common struct {
			AlwaysAllow []string `yaml:"always_allow"`
		} `yaml:"common"`
	} `yaml:"servers"`
}

// repoRelative resolves a path relative to the repo root from this package dir.
func repoRelative(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", filepath.FromSlash(rel))
}

func registeredToolNames(t *testing.T) map[string]bool {
	t.Helper()
	_, tools := testServer()
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}

func registryAlwaysAllow(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoRelative(t, "mcp/context/registry.yaml"))
	if err != nil {
		t.Fatalf("read registry.yaml: %v", err)
	}
	var reg registryFile
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("parse registry.yaml: %v", err)
	}
	for _, srv := range reg.Servers {
		if srv.Name == "agent_context" {
			if len(srv.Common.AlwaysAllow) == 0 {
				t.Fatal("agent_context server has an empty always_allow block")
			}
			return srv.Common.AlwaysAllow
		}
	}
	t.Fatal("no agent_context server entry in mcp/context/registry.yaml")
	return nil
}

// TestRegistryAlwaysAllow_MatchesRegisteredTools is (c): no stale entries.
func TestRegistryAlwaysAllow_MatchesRegisteredTools(t *testing.T) {
	t.Parallel()

	registered := registeredToolNames(t)
	var stale []string
	seen := make(map[string]bool)
	for _, name := range registryAlwaysAllow(t) {
		if seen[name] {
			t.Errorf("duplicate always_allow entry %q in registry.yaml", name)
		}
		seen[name] = true
		if !registered[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("registry.yaml always_allow references %d tool(s) mcp-agent-context does not register: %v\n"+
			"Remove them from mcp/context/registry.yaml (and mirror the change to platform/gitops).", len(stale), stale)
	}
}

// TestRegisteredTools_AreClassified is (d): every registered tool is either
// auto-approved in registry.yaml or explicitly listed as mutating here.
func TestRegisteredTools_AreClassified(t *testing.T) {
	t.Parallel()

	allowed := make(map[string]bool)
	for _, name := range registryAlwaysAllow(t) {
		allowed[name] = true
	}

	var unclassified []string
	for name := range registeredToolNames(t) {
		if allowed[name] {
			if _, denied := mutatingToolsDenylist[name]; denied {
				t.Errorf("tool %q is in BOTH registry.yaml always_allow and mutatingToolsDenylist; pick one", name)
			}
			continue
		}
		if _, denied := mutatingToolsDenylist[name]; denied {
			continue
		}
		unclassified = append(unclassified, name)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("new tool(s) %v are neither auto-approved nor classified as mutating.\n"+
			"Add each to mcp/context/registry.yaml always_allow (read-only tools) or to "+
			"mutatingToolsDenylist in this file (state-mutating tools).", unclassified)
	}
}

// TestDenylist_OnlyNamesRegisteredTools keeps the denylist itself from rotting
// once a mutating tool is deleted.
func TestDenylist_OnlyNamesRegisteredTools(t *testing.T) {
	t.Parallel()

	registered := registeredToolNames(t)
	var stale []string
	for name := range mutatingToolsDenylist {
		if !registered[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("mutatingToolsDenylist names %d unregistered tool(s): %v — drop them", len(stale), stale)
	}
}

var proxyAgentContextPattern = regexp.MustCompile(`"agent_context__(agent_[a-z0-9_]+)"`)

// TestProxyCorePatterns_OnlyNameRegisteredTools is the (e) check. cmd/loom is
// package main so it cannot be imported; the priority list is a flat slice of
// string literals, so the test reads the source instead. A pattern naming a
// tool the server does not register costs no slot but silently gives
// profile-limited vendors (codex, antigravity, Mills workers) a tool that is
// never there.
func TestProxyCorePatterns_OnlyNameRegisteredTools(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(repoRelative(t, "cmd/loom/proxy_tool_filter.go"))
	if err != nil {
		t.Fatalf("read proxy_tool_filter.go: %v", err)
	}

	matches := proxyAgentContextPattern.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no agent_context__ patterns found in cmd/loom/proxy_tool_filter.go — did the list move?")
	}

	registered := registeredToolNames(t)
	var stale []string
	for _, m := range matches {
		if !registered[m[1]] {
			stale = append(stale, m[1])
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("cmd/loom/proxy_tool_filter.go names %d unregistered agent_context tool(s): %v\n"+
			"These are silent no-ops in the llm-core / antigravity-core profiles.", len(stale), stale)
	}
}

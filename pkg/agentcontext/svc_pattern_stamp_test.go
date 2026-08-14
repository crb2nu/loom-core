package agentcontext

import (
	"context"
	"strings"
	"testing"
)

// stampMaterialsWidget is the synthetic kill-test bundle: a trivial widget
// service, one entity, in-memory storage, no auth.
func stampMaterialsWidget() map[string]any {
	return map[string]any{
		"service_name": "widget",
		"entity": map[string]any{
			"name":   "Widget",
			"fields": []any{map[string]any{"name": "Name", "type": "string"}},
		},
		"storage": "memory",
		"auth":    "none",
	}
}

// TestStamp_GoRestService_ExpandsPlan is the core S1 test: stamping the seeded
// pattern with widget materials produces a Plan whose slice files have the
// materials substituted (no leftover {{placeholders}}), and surfaces the
// required tools + resolved defaults.
func TestStamp_GoRestService_ExpandsPlan(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"project":    "services/loom-core",
		"materials":  stampMaterialsWidget(),
	})
	got := okJSON(t, res, err)

	if got["plan_id"] == nil || got["plan_id"] == "" {
		t.Fatalf("stamp returned no plan_id: %v", got)
	}
	if got["slice_count"] != float64(1) {
		t.Fatalf("slice_count = %v, want 1", got["slice_count"])
	}

	// Default substitution: module_path default resolves {{service_name}}.
	materials := got["materials"].(map[string]any)
	if materials["module_path"] != "github.com/crb2nu/widget" {
		t.Fatalf("module_path default not resolved: %v", materials["module_path"])
	}

	// Slice template expanded with materials — no leftover placeholders.
	slices := got["slices"].([]any)
	files := slices[0].(map[string]any)["files"].([]any)
	var joined string
	for _, f := range files {
		joined += f.(string) + "\n"
	}
	for _, want := range []string{"cmd/widget/main.go", "internal/repository/widget_repo.go", "internal/handler/widget_handler.go"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expanded files missing %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "{{") {
		t.Fatalf("unsubstituted placeholder left in files:\n%s", joined)
	}

	// Required tools surfaced for the (deferred) live check.
	tools := got["tools_required"].([]any)
	if len(tools) == 0 {
		t.Fatalf("expected required tools, got none")
	}

	// The stamped plan resolves by id from the store.
	pres, perr := plans.Get(ctx, map[string]any{"plan_id": got["plan_id"]})
	pgot := okJSON(t, pres, perr)
	plan := pgot["plan"].(map[string]any)
	if !strings.Contains(plan["spec_doc"].(string), "Stamped from `pattern-go-rest-service`") {
		t.Fatalf("stamped plan spec_doc missing provenance header")
	}
}

// TestStamp_GoMCPServer_ExpandsPlan stamps the Go MCP server pattern and
// asserts the expansion: tool_name-independent files substituted with the
// service_name, the tools material inlined into the slice goal, and no leftover
// placeholders.
func TestStamp_GoMCPServer_ExpandsPlan(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-mcp-server",
		"materials": map[string]any{
			"service_name": "mcp-echo",
			"tools": []any{map[string]any{
				"name":        "echo",
				"description": "Echo",
				"input_fields": []any{
					map[string]any{"name": "message", "type": "string", "required": true},
				},
			}},
		},
	})
	got := okJSON(t, res, err)

	if got["plan_id"] != "plan-stamp-go-mcp-server-mcp-echo" {
		t.Fatalf("plan_id = %v", got["plan_id"])
	}
	slices := got["slices"].([]any)
	slice := slices[0].(map[string]any)
	var joined string
	for _, f := range slice["files"].([]any) {
		joined += f.(string) + "\n"
	}
	for _, want := range []string{"cmd/mcp-echo/main.go", "internal/mcpserver/server.go", "internal/tools/registry.go"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expanded files missing %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "{{") {
		t.Fatalf("unsubstituted placeholder left in files:\n%s", joined)
	}
	// The tools material (a list) is inlined into the goal as JSON.
	if goal := slice["goal"].(string); !strings.Contains(goal, `"echo"`) || strings.Contains(goal, "{{tools}}") {
		t.Fatalf("tools material not inlined into goal: %q", goal)
	}
}

// TestStamp_GoCLI_UsesToolNamePrimary stamps the Go CLI pattern and asserts the
// plan id derives from the tool_name primary material (not the pattern slug),
// so distinct CLI stamps produce distinct plans.
func TestStamp_GoCLI_UsesToolNamePrimary(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-cli",
		"materials": map[string]any{
			"tool_name": "sprockctl",
			"commands":  []any{map[string]any{"name": "greet", "description": "Print a greeting"}},
		},
	})
	got := okJSON(t, res, err)

	if got["plan_id"] != "plan-stamp-go-cli-sprockctl" {
		t.Fatalf("plan_id should derive from tool_name; got %v", got["plan_id"])
	}
	materials := got["materials"].(map[string]any)
	if materials["module_path"] != "github.com/crb2nu/sprockctl" {
		t.Fatalf("module_path default not resolved from tool_name: %v", materials["module_path"])
	}
	slices := got["slices"].([]any)
	var joined string
	for _, f := range slices[0].(map[string]any)["files"].([]any) {
		joined += f.(string) + "\n"
	}
	if !strings.Contains(joined, "cmd/sprockctl/main.go") || strings.Contains(joined, "{{") {
		t.Fatalf("cli files not substituted:\n%s", joined)
	}
}

// TestStamp_PyFastAPI_ExpandsPlan stamps the Python FastAPI pattern: the app/
// package files are material-independent (no placeholders to substitute) and
// the entity object still drives goal expansion.
func TestStamp_PyFastAPI_ExpandsPlan(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-python-fastapi-service",
		"materials": map[string]any{
			"service_name": "widget-api",
			"entity": map[string]any{
				"name":   "Widget",
				"fields": []any{map[string]any{"name": "name", "type": "str"}},
			},
		},
	})
	got := okJSON(t, res, err)

	if got["plan_id"] != "plan-stamp-python-fastapi-service-widget-api" {
		t.Fatalf("plan_id = %v", got["plan_id"])
	}
	slices := got["slices"].([]any)
	slice := slices[0].(map[string]any)
	var joined string
	for _, f := range slice["files"].([]any) {
		joined += f.(string) + "\n"
	}
	for _, want := range []string{"app/main.py", "app/errors.py", "tests/test_api.py", "uv.lock"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expanded files missing %q; got:\n%s", want, joined)
		}
	}
	// entity.name derived sub lands in the goal.
	if goal := slice["goal"].(string); !strings.Contains(goal, "entity Widget") {
		t.Fatalf("entity.name not substituted into goal: %q", goal)
	}
}

// TestStamp_MissingRequiredMaterial rejects materials missing a required field.
func TestStamp_MissingRequiredMaterial(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, _ := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"materials":  map[string]any{"storage": "memory"}, // no service_name / entity
	})
	if res == nil || !res.IsError {
		t.Fatalf("expected error for missing required material, got %v", res)
	}
}

// TestStamp_InvalidEnum rejects an out-of-enum material value.
func TestStamp_InvalidEnum(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	m := stampMaterialsWidget()
	m["storage"] = "mysql" // not in {memory, postgres, sqlite}
	res, _ := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"materials":  m,
	})
	if res == nil || !res.IsError {
		t.Fatalf("expected error for invalid enum, got %v", res)
	}
}

// TestStamp_UnknownPattern errors when the pattern does not exist.
func TestStamp_UnknownPattern(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	res, _ := stampPattern(context.Background(), patterns, plans, map[string]any{
		"pattern_id": "pattern-does-not-exist",
		"materials":  stampMaterialsWidget(),
	})
	if res == nil || !res.IsError {
		t.Fatalf("expected error for unknown pattern, got %v", res)
	}
}

// TestStamp_ToolsProbe_AbortsOnMissing: when available_tools is supplied and a
// required tool (gitlab) is absent, the stamp aborts loudly and creates no Plan.
func TestStamp_ToolsProbe_AbortsOnMissing(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, _ := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id":      "pattern-go-rest-service",
		"materials":       stampMaterialsWidget(),
		"available_tools": []any{"go", "devbox"}, // gitlab (required) is missing
	})
	if res == nil || !res.IsError {
		t.Fatalf("expected abort when a required tool is missing, got %v", res)
	}
	if msg := res.Content[0].Text; !strings.Contains(msg, "gitlab") {
		t.Fatalf("error should name the missing tool gitlab; got: %s", msg)
	}
	// No Plan should have been created for the aborted stamp.
	if pres, _ := plans.Get(ctx, map[string]any{"plan_id": "plan-stamp-go-rest-service-widget"}); pres != nil && !pres.IsError {
		t.Fatalf("aborted stamp must not create a Plan")
	}
}

// TestStamp_ToolsProbe_PassesWhenAllPresent: all required tools present → the
// stamp succeeds and reports tools_probed.
func TestStamp_ToolsProbe_PassesWhenAllPresent(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id":      "pattern-go-rest-service",
		"materials":       stampMaterialsWidget(),
		"available_tools": []any{"go", "devbox", "gitlab", "flux"},
	})
	got := okJSON(t, res, err)
	if got["tools_probed"] != true {
		t.Fatalf("tools_probed = %v, want true", got["tools_probed"])
	}
	if got["plan_id"] == "" {
		t.Fatalf("expected a plan_id on a satisfied stamp")
	}
}

// TestStamp_ToolsProbe_NamespacedNames: lenient matching accepts MCP-namespaced
// tool names (devbox satisfied by devbox__devbox_status, etc.).
func TestStamp_ToolsProbe_NamespacedNames(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id":      "pattern-go-rest-service",
		"materials":       stampMaterialsWidget(),
		"available_tools": []any{"go", "devbox__devbox_status", "gitlab__verify_token"},
	})
	okJSON(t, res, err) // no error == all required tools matched
}

// TestStamp_NoProbeWhenAbsent: without available_tools the manifest is surfaced
// but not gated (back-compat) — tools_probed is false.
func TestStamp_NoProbeWhenAbsent(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"materials":  stampMaterialsWidget(),
	})
	got := okJSON(t, res, err)
	if got["tools_probed"] != false {
		t.Fatalf("tools_probed = %v, want false (no available_tools)", got["tools_probed"])
	}
}

// TestStamp_TargetDir_PrefixesFiles: a target_dir material confines every
// expanded slice file under the given subdirectory and grounds the slice goal,
// so a stamp into an existing monorepo cannot clobber the host's root files.
func TestStamp_TargetDir_PrefixesFiles(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	materials := stampMaterialsWidget()
	materials["target_dir"] = "examples/{{service_name}}/"

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"materials":  materials,
	})
	got := okJSON(t, res, err)

	slices := got["slices"].([]any)
	slice := slices[0].(map[string]any)
	for _, f := range slice["files"].([]any) {
		if !strings.HasPrefix(f.(string), "examples/widget/") {
			t.Fatalf("file %q not confined under target_dir", f)
		}
	}
	if goal := slice["goal"].(string); !strings.Contains(goal, "under `examples/widget/`") {
		t.Fatalf("goal not grounded with target_dir: %q", goal)
	}
	if got["materials"].(map[string]any)["target_dir"] != "examples/widget" {
		t.Fatalf("normalized target_dir = %v, want examples/widget", got["materials"].(map[string]any)["target_dir"])
	}
}

// TestStamp_TargetDir_NoPrefixWhenAbsent: without target_dir the expansion is
// unchanged (fresh-repo case) — files stay repo-root relative, goal ungrounded.
func TestStamp_TargetDir_NoPrefixWhenAbsent(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	res, err := stampPattern(ctx, patterns, plans, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"materials":  stampMaterialsWidget(),
	})
	got := okJSON(t, res, err)

	slices := got["slices"].([]any)
	slice := slices[0].(map[string]any)
	var sawRootGoMod bool
	for _, f := range slice["files"].([]any) {
		if f.(string) == "go.mod" {
			sawRootGoMod = true
		}
	}
	if !sawRootGoMod {
		t.Fatalf("fresh-repo stamp should keep repo-root go.mod: %v", slice["files"])
	}
	if goal := slice["goal"].(string); strings.Contains(goal, "host repository") {
		t.Fatalf("goal should not carry target_dir grounding when absent: %q", goal)
	}
}

// TestStamp_TargetDir_RejectsEscapes: absolute and repo-escaping target_dir
// values abort the stamp before any Plan is written.
func TestStamp_TargetDir_RejectsEscapes(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	for _, td := range []string{"/etc/widget", "../outside", "examples/../../outside"} {
		materials := stampMaterialsWidget()
		materials["target_dir"] = td
		res, err := stampPattern(ctx, patterns, plans, map[string]any{
			"pattern_id": "pattern-go-rest-service",
			"materials":  materials,
		})
		if err != nil {
			t.Fatalf("target_dir %q: transport error: %v", td, err)
		}
		if !res.IsError {
			t.Fatalf("target_dir %q: expected stamp to abort", td)
		}
	}
}

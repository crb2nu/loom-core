package agentcontext

// manifest_dump_test.go -- kill-test helper (skipped unless MANIFEST_DUMP_DIR
// is set): dumps the exact stamped manifest (plan spec_doc + expanded slice)
// for each expansion builtin pattern, so pattern kill-tests can hand
// context-free follower agents the real stamp bytes rather than a
// reconstruction. See .loom/kill-test-pattern-expansion-2026-07-03.md.
// Run: MANIFEST_DUMP_DIR=<dir> go test ./pkg/agentcontext -run TestDumpStampManifests -v

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDumpStampManifests(t *testing.T) {
	dir := os.Getenv("MANIFEST_DUMP_DIR")
	if dir == "" {
		t.Skip("MANIFEST_DUMP_DIR not set")
	}
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	patterns := newTestPatternSvc()
	plans := newTestPlanSvc()
	ctx := context.Background()
	patterns.SeedBuiltins(ctx)

	cases := []struct {
		file      string
		patternID string
		materials map[string]any
	}{
		{
			file:      "manifest-go-mcp-server.md",
			patternID: "pattern-go-mcp-server",
			materials: map[string]any{
				"service_name": "mcp-echo",
				"tools": []any{
					map[string]any{
						"name":        "echo",
						"description": "Echo the supplied message",
						"input_fields": []any{
							map[string]any{"name": "message", "type": "string", "required": true, "description": "Text to echo"},
						},
					},
					map[string]any{
						"name":        "add",
						"description": "Add two integers",
						"input_fields": []any{
							map[string]any{"name": "a", "type": "int", "required": true, "description": "First addend"},
							map[string]any{"name": "b", "type": "int", "required": true, "description": "Second addend"},
						},
					},
				},
			},
		},
		{
			file:      "manifest-go-cli.md",
			patternID: "pattern-go-cli",
			materials: map[string]any{
				"tool_name": "sprockctl",
				"commands": []any{
					map[string]any{
						"name":        "greet",
						"description": "Print a greeting",
						"flags": []any{
							map[string]any{"name": "name", "type": "string", "default": "world", "description": "Who to greet"},
							map[string]any{"name": "shout", "type": "bool", "default": "false", "description": "Uppercase the greeting"},
						},
					},
					map[string]any{
						"name":        "count",
						"description": "Count to n",
						"flags": []any{
							map[string]any{"name": "n", "type": "int", "default": "3", "description": "Upper bound"},
						},
					},
				},
			},
		},
		{
			file:      "manifest-python-fastapi.md",
			patternID: "pattern-python-fastapi-service",
			materials: map[string]any{
				"service_name": "widget-api",
				"entity": map[string]any{
					"name": "Widget",
					"fields": []any{
						map[string]any{"name": "name", "type": "str"},
						map[string]any{"name": "quantity", "type": "int"},
					},
				},
			},
		},
		// Dobby cards (schema_patterns_loomcore.go). The runbook/metric/panel
		// cases use synthetic-but-plausible materials (the follower's diff is
		// judged, never merged); the endpoint case deliberately uses the REAL
		// relaunch-candidates demand item so a faithful follower produces a
		// genuinely shippable diff, mirroring how !874 made the go-rest
		// kill-test earn a real merge.
		{
			file:      "manifest-loom-runbook.md",
			patternID: "pattern-loom-runbook",
			materials: map[string]any{
				"topic":    "WidgetDB connection-pool exhaustion during eval ingest",
				"doc_slug": "widgetdb-pool-exhaustion",
				"shape":    "signature-major",
				"signatures": []any{
					map[string]any{
						"name":         "pq: sorry, too many clients already",
						"detection":    "eval ingest 500s; operator logs show pq connection errors within the ingest window",
						"owner":        "WidgetDB/platform data owner",
						"safe_actions": "read-only pool stats query; bounded log pulls; never restart the database",
						"verification": "ingest resumes and pool utilization stays under 80% for 30m",
					},
					map[string]any{
						"name":         "context deadline exceeded acquiring pool connection",
						"detection":    "p95 acquire latency alert; ingest retries visible in logs",
						"owner":        "WidgetDB/platform data owner",
						"safe_actions": "bounded log pulls; confirm pool_max against configured replicas",
						"verification": "acquire p95 back under 100ms for 30m",
					},
				},
				"related_docs": []any{"docs/mills-escalation-and-dependency-failures.md"},
			},
		},
		{
			file:      "manifest-mills-metric.md",
			patternID: "pattern-mills-metric",
			materials: map[string]any{
				"metric_name": "mills_canary_polish_total",
				"metric_slug": "canary-polish-metric",
				"collector":   "counter_vec",
				"labels": []any{
					map[string]any{"name": "outcome", "values": []any{"polished", "scuffed", "dropped"}},
				},
				"help":      "Canary polish attempts, by outcome (polished/scuffed/dropped).",
				"call_site": "pkg/mills/canary_scheduler.go",
				"rationale": "canary polish outcomes were invisible in the 24h KPI window, so a silent canary regression looked like idle demand",
			},
		},
		{
			file:      "manifest-operator-read-endpoint.md",
			patternID: "pattern-operator-read-endpoint",
			materials: map[string]any{
				"topic":          "escalations",
				"route_path":     "escalations/relaunch-candidates",
				"endpoint_slug":  "relaunch-candidates-endpoint",
				"projection":     "escalated backlog items whose LATEST pipeline run has EscalationRetryable=true, projected as {ID, Title, EscalationClass, FailureClass, EndedAt}",
				"response_shape": "bare-array",
				"limit_default":  "50",
				"limit_max":      "200",
			},
		},
		{
			file:      "manifest-hud-panel.md",
			patternID: "pattern-hud-panel",
			materials: map[string]any{
				"panel_name":       "Grades",
				"panel_slug":       "hud-grades-panel",
				"subview_id":       "mills-grades",
				"subview_label":    "Grades",
				"subview_key":      "j",
				"subview_group":    "Mill floor",
				"data_route":       "/api/mills/eval/scores",
				"store_domain":     "grades",
				"poll_interval_ms": "15000",
			},
		},
	}

	for _, c := range cases {
		res, err := stampPattern(ctx, patterns, plans, map[string]any{
			"pattern_id": c.patternID,
			"materials":  c.materials,
		})
		got := okJSON(t, res, err)
		planID, _ := got["plan_id"].(string)
		pres, perr := plans.Get(ctx, map[string]any{"plan_id": planID})
		pgot := okJSON(t, pres, perr)
		plan := pgot["plan"].(map[string]any)
		specDoc, _ := plan["spec_doc"].(string)

		out := specDoc + "\n\n## Slices\n\n"
		for _, s := range got["slices"].([]any) {
			sm := s.(map[string]any)
			out += fmt.Sprintf("### %s\n\n%s\n\nFiles:\n", sm["name"], sm["goal"])
			for _, f := range sm["files"].([]any) {
				out += fmt.Sprintf("- `%s`\n", f)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, c.file), []byte(out), 0o644); err != nil {
			t.Fatalf("write %s: %v", c.file, err)
		}
		t.Logf("wrote %s (plan %s)", c.file, planID)
	}
}

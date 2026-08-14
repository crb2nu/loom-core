package agentcontext

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func newTestPatternSvc() *PatternSvc {
	// nil Qdrant/embedder exercises the in-memory cache path (Qdrant-first fetch
	// falls back to the cache), matching newTestPlanSvc.
	return NewPatternSvc(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestPattern_AddGetRoundTrip verifies a Pattern with nested structures
// (materials_schema, pins, gauge) round-trips through add→get intact, and that
// the id is the stable human-meaningful pattern-<slug>.
func TestPattern_AddGetRoundTrip(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()

	res, err := ps.Add(ctx, map[string]any{
		"name":     "Go REST service",
		"makes":    "Go REST microservice",
		"status":   "approved",
		"agent_id": "claude-A",
		"materials_schema": []any{
			map[string]any{"name": "service_name", "type": "string", "required": true},
			map[string]any{"name": "storage", "type": "enum", "enum": []any{"memory", "postgres"}},
		},
		"pins": []any{
			map[string]any{"axis": "transport", "value": "net/http"},
		},
		"gauge": map[string]any{
			"commands":   []any{"go build ./..."},
			"assertions": []any{"GET /healthz -> 200"},
		},
	})
	created := okJSON(t, res, err)

	if created["pattern_id"] != "pattern-go-rest-service" {
		t.Fatalf("pattern_id = %v, want pattern-go-rest-service", created["pattern_id"])
	}

	// Retrieve with NO agent_id — the catalog is cross-agent.
	res, err = ps.Get(ctx, map[string]any{"pattern_id": "pattern-go-rest-service"})
	got := okJSON(t, res, err)
	p, ok := got["pattern"].(map[string]any)
	if !ok {
		t.Fatalf("get returned no pattern: %v", got)
	}
	if p["makes"] != "Go REST microservice" {
		t.Fatalf("makes = %v", p["makes"])
	}
	if p["created_by"] != "claude-A" {
		t.Fatalf("created_by attribution lost: %v", p["created_by"])
	}
	ms, ok := p["materials_schema"].([]any)
	if !ok || len(ms) != 2 {
		t.Fatalf("materials_schema did not round-trip: %v", p["materials_schema"])
	}
	first := ms[0].(map[string]any)
	if first["name"] != "service_name" || first["required"] != true {
		t.Fatalf("materials_schema field lost: %v", first)
	}
	pins, ok := p["pins"].([]any)
	if !ok || len(pins) != 1 {
		t.Fatalf("pins did not round-trip: %v", p["pins"])
	}
	gauge, ok := p["gauge"].(map[string]any)
	if !ok || gauge["commands"] == nil {
		t.Fatalf("gauge did not round-trip: %v", p["gauge"])
	}
}

// TestPattern_InvalidStatus rejects an unknown status.
func TestPattern_InvalidStatus(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	res, err := ps.Add(context.Background(), map[string]any{
		"name": "x", "makes": "y", "status": "bogus",
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected error result for invalid status, got %v", res)
	}
}

// TestPattern_RequiresNameAndMakes enforces the two required fields.
func TestPattern_RequiresNameAndMakes(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	res, _ := ps.Add(context.Background(), map[string]any{"name": "only-name"})
	if res == nil || !res.IsError {
		t.Fatalf("expected error result when makes is missing, got %v", res)
	}
}

// TestPattern_ListFilterByStatus checks status filtering via the cache path.
func TestPattern_ListFilterByStatus(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()

	r1, e1 := ps.Add(ctx, map[string]any{"name": "approved one", "makes": "svc", "status": "approved"})
	okJSON(t, r1, e1)
	r2, e2 := ps.Add(ctx, map[string]any{"name": "candidate one", "makes": "svc", "status": "candidate"})
	okJSON(t, r2, e2)

	res, err := ps.List(ctx, map[string]any{"status": "approved"})
	got := okJSON(t, res, err)
	if got["count"] != float64(1) {
		t.Fatalf("count = %v, want 1 (status=approved)", got["count"])
	}
	list := got["patterns"].([]any)
	if list[0].(map[string]any)["status"] != "approved" {
		t.Fatalf("filtered wrong status: %v", list[0])
	}
}

// TestPattern_SeedBuiltins_CatalogWellFormed asserts every builtin pattern
// seeds, resolves by id, and is structurally complete: valid status, pins,
// materials, a gauge, a slice template, and composed engrams that exist in the
// builtin engram catalog (pattern <-> engram non-drift). It also validates that
// every {{placeholder}} a slice template uses is derivable from the pattern's
// own materials schema, so a stamp can never leave an unsubstituted token.
func TestPattern_SeedBuiltins_CatalogWellFormed(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()
	ps.SeedBuiltins(ctx)

	builtins := BuiltinPatterns()
	if len(builtins) < 4 {
		t.Fatalf("expected the expanded builtin catalog (>=4 patterns), got %d", len(builtins))
	}

	// The engram catalog every pattern may compose from.
	engramCatalog := map[string]bool{}
	for _, e := range allBuiltinEngrams() {
		engramCatalog[e.uri()] = true
	}

	placeholderRe := regexp.MustCompile(`\{\{([^}]+)\}\}`)
	for _, want := range builtins {
		res, err := ps.Get(ctx, map[string]any{"pattern_id": want.ID})
		got := okJSON(t, res, err)
		p, ok := got["pattern"].(map[string]any)
		if !ok {
			t.Fatalf("%s: seeded pattern did not resolve", want.ID)
		}
		if !patternStatusValid(p["status"].(string)) {
			t.Fatalf("%s: invalid status %v", want.ID, p["status"])
		}
		if len(want.Pins) < 8 {
			t.Fatalf("%s: only %d pins — builtins must close the stampability seams", want.ID, len(want.Pins))
		}
		if len(want.MaterialsSchema) == 0 || want.Gauge == nil || len(want.SliceTemplate) == 0 {
			t.Fatalf("%s: missing materials/gauge/slice_template", want.ID)
		}
		if len(want.Engrams) == 0 {
			t.Fatalf("%s: composes no engrams", want.ID)
		}
		for _, uri := range want.Engrams {
			if !engramCatalog[uri] {
				t.Fatalf("%s: composed engram %s not in the builtin engram catalog (drift)", want.ID, uri)
			}
		}

		// Every slice-template placeholder must be derivable from the schema:
		// a field name, <object-field>.name, or <object-field>_lower.
		derivable := map[string]bool{"target_dir": true}
		for _, f := range want.MaterialsSchema {
			derivable[f.Name] = true
			if f.Type == "object" {
				derivable[f.Name+".name"] = true
				derivable[f.Name+"_lower"] = true
			}
		}
		for _, tpl := range want.SliceTemplate {
			blob := tpl.Name + "\n" + tpl.Goal + "\n" + tpl.AcceptanceCriteria + "\n" + strings.Join(tpl.Files, "\n")
			for _, m := range placeholderRe.FindAllStringSubmatch(blob, -1) {
				if !derivable[m[1]] {
					t.Fatalf("%s: slice template references {{%s}} which no material derives", want.ID, m[1])
				}
			}
			// Every slice's composed engrams must also exist.
			for _, uri := range tpl.Engrams {
				if !engramCatalog[uri] {
					t.Fatalf("%s: slice %q engram %s not in catalog", want.ID, tpl.Name, uri)
				}
			}
		}
	}
}

// TestPattern_SeedBuiltins asserts the seed catalog lands and the go-rest-service
// pattern is well-formed (pins + materials present), satisfying S0's "one seeded
// pattern resolves by id" acceptance.
func TestPattern_SeedBuiltins(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()

	ps.SeedBuiltins(ctx)

	res, err := ps.Get(ctx, map[string]any{"pattern_id": "pattern-go-rest-service"})
	got := okJSON(t, res, err)
	p := got["pattern"].(map[string]any)
	if p["status"] != "approved" {
		t.Fatalf("seed status = %v, want approved", p["status"])
	}
	pins, ok := p["pins"].([]any)
	if !ok || len(pins) < 10 {
		t.Fatalf("seed should pin all load-bearing + stampability axes; got %d pins", len(pins))
	}
	ms, ok := p["materials_schema"].([]any)
	if !ok || len(ms) == 0 {
		t.Fatalf("seed missing materials_schema: %v", p["materials_schema"])
	}
	// The stampability seams from the S1 kill-test must be pinned.
	wantAxes := map[string]bool{"error_envelope": false, "wiring": false, "error_model": false, "status_codes": false, "endpoint_bodies": false}
	for _, pin := range pins {
		axis, _ := pin.(map[string]any)["axis"].(string)
		if _, ok := wantAxes[axis]; ok {
			wantAxes[axis] = true
		}
	}
	for axis, seen := range wantAxes {
		if !seen {
			t.Fatalf("seed pattern missing required stampability pin: %s", axis)
		}
	}
}

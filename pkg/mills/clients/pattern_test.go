package clients

import (
	"context"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// --- Prompt injection (Pattern Loom A1) -------------------------------------

func TestBuildCouncilEditorPrompt_IncludesPatternCatalog(t *testing.T) {
	brief := &council.Brief{Markdown: "Ship the thing."}
	patterns := []council.PatternRef{
		{ID: "pattern-go-rest-service", Makes: "Go REST microservice", Name: "Go REST service"},
		{ID: "pattern-mcp-server", Makes: "Loom MCP server", Name: "MCP server"},
	}
	prompt := buildCouncilEditorPrompt(brief, nil, patterns, "", "")

	if !strings.Contains(prompt, "## Approved patterns") {
		t.Errorf("prompt missing approved-patterns section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- pattern-go-rest-service (Go REST microservice)") {
		t.Errorf("prompt missing first pattern listing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- pattern-mcp-server (Loom MCP server)") {
		t.Errorf("prompt missing second pattern listing:\n%s", prompt)
	}
	// The JSON shape must advertise the new field so the editor emits it.
	if !strings.Contains(prompt, `"pattern_id"`) {
		t.Errorf("prompt JSON shape missing pattern_id field:\n%s", prompt)
	}
	// Brief still present.
	if !strings.Contains(prompt, "Ship the thing.") {
		t.Errorf("prompt dropped brief body:\n%s", prompt)
	}
}

func TestBuildCouncilEditorPrompt_OmitsCatalogWhenEmpty(t *testing.T) {
	brief := &council.Brief{Markdown: "Ship the thing."}
	prompt := buildCouncilEditorPrompt(brief, nil, nil, "", "")

	if strings.Contains(prompt, "## Approved patterns") {
		t.Errorf("empty catalog should not render the section:\n%s", prompt)
	}
	// The pattern_id field stays advisory in the JSON shape (always present),
	// but no catalog listing should appear.
	if strings.Contains(prompt, "- pattern-") {
		t.Errorf("empty catalog should not list any pattern ids:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Ship the thing.") {
		t.Errorf("prompt dropped brief body:\n%s", prompt)
	}
}

func TestBuildPatternCatalogSection_FallsBackToName(t *testing.T) {
	// Makes empty → fall back to Name; both empty → bare id.
	patterns := []council.PatternRef{
		{ID: "pattern-a", Name: "Alpha"},
		{ID: "pattern-b"},
		{ID: "   "}, // skipped (empty id)
	}
	got := buildPatternCatalogSection(patterns)
	if !strings.Contains(got, "- pattern-a (Alpha)") {
		t.Errorf("expected name fallback for pattern-a:\n%s", got)
	}
	if !strings.Contains(got, "- pattern-b\n") {
		t.Errorf("expected bare id for pattern-b:\n%s", got)
	}
	if strings.Contains(got, "pattern- (") || strings.Contains(got, "-   ") {
		t.Errorf("empty-id pattern should be skipped:\n%s", got)
	}
}

// --- Proposal parsing (Pattern Loom A1) -------------------------------------

func TestParseCouncilProposals_CarriesPatternID(t *testing.T) {
	raw := "## Backlog Proposals\n```json\n" + `{"proposals": [
  {"title": "Add X", "priority": "P1", "pattern_id": "pattern-go-rest-service",
   "slices": [{"name": "s", "goal": "g", "files": ["a.go"]}]},
  {"title": "Add Y", "pattern_id": "none"}
]}` + "\n```\n"
	got := parseCouncilProposals(raw)
	if len(got) != 2 {
		t.Fatalf("proposals=%d, want 2", len(got))
	}
	if got[0].PatternID != "pattern-go-rest-service" {
		t.Errorf("proposal[0].PatternID=%q, want pattern-go-rest-service", got[0].PatternID)
	}
	if got[1].PatternID != "none" {
		t.Errorf("proposal[1].PatternID=%q, want none", got[1].PatternID)
	}
}

func TestParseCouncilProposals_AbsentPatternIDIsEmpty(t *testing.T) {
	raw := "## Backlog Proposals\n" + `{"proposals": [{"title": "No pattern field"}]}`
	got := parseCouncilProposals(raw)
	if len(got) != 1 {
		t.Fatalf("proposals=%d, want 1", len(got))
	}
	if got[0].PatternID != "" {
		t.Errorf("PatternID=%q, want empty when absent", got[0].PatternID)
	}
}

// --- PatternClient read path (Pattern Loom A1) ------------------------------

func TestPatternClient_ListApprovedPatterns(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": makeCallToolResult(t, map[string]any{
				"ok":    true,
				"count": 2,
				"patterns": []map[string]any{
					{"id": "pattern-go-rest-service", "makes": "Go REST microservice", "name": "Go REST service", "status": "approved"},
					{"id": "", "makes": "skip-empty-id"}, // dropped
					{"id": "pattern-mcp-server", "makes": "Loom MCP server", "name": "MCP server", "status": "approved"},
				},
			}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPatternClient(hub)

	got, err := pc.ListApprovedPatterns(context.Background())
	if err != nil {
		t.Fatalf("ListApprovedPatterns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("patterns=%d, want 2 (empty-id dropped)", len(got))
	}
	if got[0].ID != "pattern-go-rest-service" || got[0].Makes != "Go REST microservice" || got[0].Name != "Go REST service" {
		t.Errorf("pattern[0]=%+v", got[0])
	}
	if got[1].ID != "pattern-mcp-server" {
		t.Errorf("pattern[1].ID=%q", got[1].ID)
	}

	// Assert the call carried status=approved.
	var sawStatus bool
	for _, m := range ft.sentMessages() {
		if strings.Contains(string(m.Params), `"status":"approved"`) {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Errorf("expected agent_pattern_list call with status=approved; sent=%v", ft.sentMessages())
	}
}

func TestPatternClient_NilHubIsBestEffort(t *testing.T) {
	pc := &PatternClient{} // no hub
	got, err := pc.ListApprovedPatterns(context.Background())
	if err != nil {
		t.Errorf("nil hub must not error (best-effort): %v", err)
	}
	if got != nil {
		t.Errorf("nil hub should return nil patterns, got %d", len(got))
	}
}

func TestPatternClient_RecordInstanceCarriesOptionalGrade(t *testing.T) {
	for _, tc := range []struct {
		name, grade string
		wantGrade   bool
	}{{"graded", "keep", true}, {"ungraded", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTransport{responses: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
				"tools/call": makeCallToolResult(t, map[string]any{"ok": true, "instances_shipped_green": 2, "status": "approved"}),
			}}
			pc := NewPatternClient(newTestHubClient(t, ft))
			if _, err := pc.RecordGradedInstance(context.Background(), "pattern-x", "!1", "loom-core", tc.grade); err != nil {
				t.Fatal(err)
			}
			var args string
			for _, m := range ft.sentMessages() {
				if strings.Contains(string(m.Params), "agent_pattern_record_instance") {
					args = string(m.Params)
				}
			}
			has := strings.Contains(args, `"grade":"keep"`)
			if has != tc.wantGrade {
				t.Fatalf("args=%s grade presence=%v want=%v", args, has, tc.wantGrade)
			}
		})
	}
}

// fetchApprovedPatterns must swallow lister errors so a council run never
// blocks on a pattern fetch.
type erroringLister struct{}

func (erroringLister) ListApprovedPatterns(context.Context) ([]council.PatternRef, error) {
	return nil, context.DeadlineExceeded
}

func TestFetchApprovedPatterns_SwallowsErrors(t *testing.T) {
	if got := fetchApprovedPatterns(context.Background(), nil); got != nil {
		t.Errorf("nil lister should yield nil, got %v", got)
	}
	if got := fetchApprovedPatterns(context.Background(), erroringLister{}); got != nil {
		t.Errorf("erroring lister should yield nil (best-effort), got %v", got)
	}
}

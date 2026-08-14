package clients

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestParseCouncilProposals_ValidBlock(t *testing.T) {
	raw := "## Research\nr\n## Product Spec\ns\n## Implementation Plan\nplan body\n## Backlog Proposals\n```json\n" +
		`{"proposals": [
  {"title": "Add X", "priority": "P1", "labels": ["docs"],
   "slices": [
     {"name": "slice a", "goal": "do a", "files": ["a.go"]},
     {"name": "slice b", "goal": "do b", "files": ["b.go"]}
   ]}
]}` + "\n```\n"
	got := parseCouncilProposals(raw)
	if len(got) != 1 {
		t.Fatalf("proposals=%d, want 1", len(got))
	}
	p := got[0]
	if p.Title != "Add X" {
		t.Errorf("title=%q", p.Title)
	}
	if p.Priority != store.P1 {
		t.Errorf("priority=%q, want P1", p.Priority)
	}
	if len(p.PlanSlices) != 2 {
		t.Fatalf("slices=%d, want 2", len(p.PlanSlices))
	}
	if p.PlanSlices[0].Name != "slice a" || p.PlanSlices[0].Goal != "do a" || len(p.PlanSlices[0].Files) != 1 {
		t.Errorf("slice0=%+v", p.PlanSlices[0])
	}
}

func TestParseCouncilProposals_NoBlock(t *testing.T) {
	raw := "## Research\n...\n## Implementation Plan\n...no proposals section..."
	if got := parseCouncilProposals(raw); got != nil {
		t.Fatalf("want nil (no block), got %d", len(got))
	}
}

func TestParseCouncilProposals_Malformed(t *testing.T) {
	raw := "## Backlog Proposals\n```json\n{not valid json,,,}\n```"
	if got := parseCouncilProposals(raw); got != nil {
		t.Fatalf("want nil on malformed JSON, got %d", len(got))
	}
}

func TestParseCouncilProposals_DefaultsAndSkips(t *testing.T) {
	raw := "## Backlog Proposals\n" + `{"proposals": [
  {"title": "", "slices": [{"name": "x"}]},
  {"title": "Keep me", "priority": "ZZ",
   "slices": [{"name": "", "goal": "skip-empty-name"}, {"name": "keep", "goal": "g"}]}
]}`
	got := parseCouncilProposals(raw)
	// First proposal dropped (empty title); second kept with default P2 + one valid slice.
	if len(got) != 1 {
		t.Fatalf("proposals=%d, want 1", len(got))
	}
	if got[0].Priority != store.P2 {
		t.Errorf("priority=%q, want P2 default for unrecognized", got[0].Priority)
	}
	if len(got[0].PlanSlices) != 1 || got[0].PlanSlices[0].Name != "keep" {
		t.Errorf("slices=%+v, want one named 'keep'", got[0].PlanSlices)
	}
}

// TestParseCouncil_ObjectiveAndConnectiveTissue proves a fable-style editor
// output (a plan-level objective + per-slice depends_on / interface_contracts /
// acceptance_criteria) parses into the objective and the slice tissue. This is
// the wire-format half of the riskiest-assumption kill-test: the contract the
// frame is asked to fill round-trips into typed fields the spin threads onward.
func TestParseCouncil_ObjectiveAndConnectiveTissue(t *testing.T) {
	raw := "## Research\nr\n## Product Spec\ns\n## Implementation Plan\nplan body\n## Backlog Proposals\n```json\n" +
		`{"objective": "Give a couple one shared source of truth for their household; slice 1 lands the store the rest code against, then the API and both clients layer on top.",
  "proposals": [
  {"title": "Build FamilyForge", "priority": "P1",
   "slices": [
     {"name": "schema", "goal": "define the shared store", "files": ["pkg/store/schema.go"],
      "interface_contracts": "publishes the FamilyStore schema later slices code against",
      "acceptance_criteria": "schema round-trips"},
     {"name": "api", "goal": "wire the API", "files": ["pkg/api/api.go"],
      "depends_on": ["schema"],
      "interface_contracts": "consumes FamilyStore; exposes REST"},
     {"name": "ios", "goal": "build the iOS client", "files": ["ios/App.swift"],
      "depends_on": ["api", "schema"]}
   ]}
]}` + "\n```\n"

	if obj := parseCouncilObjective(raw); !strings.Contains(obj, "shared source of truth") {
		t.Errorf("objective = %q, want the synthesized through-line", obj)
	}

	got := parseCouncilProposals(raw)
	if len(got) != 1 || len(got[0].PlanSlices) != 3 {
		t.Fatalf("proposals/slices = %d/%v", len(got), got)
	}
	sl := got[0].PlanSlices
	if sl[0].InterfaceContracts == "" || sl[0].AcceptanceCriteria == "" {
		t.Errorf("schema slice tissue dropped: %+v", sl[0])
	}
	if len(sl[1].DependsOn) != 1 || sl[1].DependsOn[0] != "schema" {
		t.Errorf("api.depends_on = %v, want [schema] (by name)", sl[1].DependsOn)
	}
	if len(sl[2].DependsOn) != 2 || sl[2].DependsOn[0] != "api" || sl[2].DependsOn[1] != "schema" {
		t.Errorf("ios.depends_on = %v, want [api schema]", sl[2].DependsOn)
	}
}

// TestParseCouncilObjective_Absent proves an editor that omits the objective
// (or the whole block) yields "" rather than an error — the graceful-absence
// contract that keeps older/sparse plans clean.
func TestParseCouncilObjective_Absent(t *testing.T) {
	for _, raw := range []string{
		"## Research\nno proposals block at all",
		"## Backlog Proposals\n```json\n" + `{"proposals":[{"title":"X","slices":[{"name":"s","files":["a.go"]}]}]}` + "\n```",
		"## Backlog Proposals\n```json\n{not valid json,,,}\n```",
	} {
		if obj := parseCouncilObjective(raw); obj != "" {
			t.Errorf("objective = %q, want empty for %q", obj, raw)
		}
	}
}

// TestParseCouncilProposalsDiag_Statuses pins the diagnostic classification that
// makes a council run's created:0 self-explaining, and asserts councilProposalsNote
// is empty iff proposals were emitted and surfaces the model's omit_reason.
func TestParseCouncilProposalsDiag_Statuses(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantStatus proposalParseStatus
		wantN      int
		wantDetail string // substring of detail; "" = skip
	}{
		{
			name:       "emitted",
			raw:        "## Backlog Proposals\n```json\n" + `{"proposals":[{"title":"Add X","slices":[{"name":"s","files":["a.go"]}]}]}` + "\n```",
			wantStatus: proposalsEmitted,
			wantN:      1,
		},
		{
			name:       "marker absent",
			raw:        "## Research\nr\n## Implementation Plan\nno proposals section here",
			wantStatus: proposalsMarkerAbsent,
		},
		{
			name:       "no json object",
			raw:        "## Backlog Proposals\njust prose, no braces at all",
			wantStatus: proposalsNoJSON,
		},
		{
			name:       "decode error",
			raw:        "## Backlog Proposals\n```json\n{not valid,,,}\n```",
			wantStatus: proposalsDecodeError,
		},
		{
			name:       "empty declared with reason",
			raw:        "## Backlog Proposals\n```json\n" + `{"proposals":[],"omit_reason":"single merge-sized unit"}` + "\n```",
			wantStatus: proposalsEmptyDeclared,
			wantDetail: "single merge-sized unit",
		},
		{
			name:       "empty titles",
			raw:        "## Backlog Proposals\n```json\n" + `{"proposals":[{"title":"   ","slices":[{"name":"s"}]}]}` + "\n```",
			wantStatus: proposalsEmptyTitles,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, status, detail := parseCouncilProposalsDiag(tt.raw)
			if status != tt.wantStatus {
				t.Fatalf("status=%q, want %q", status, tt.wantStatus)
			}
			if len(got) != tt.wantN {
				t.Errorf("proposals=%d, want %d", len(got), tt.wantN)
			}
			if tt.wantDetail != "" && !strings.Contains(detail, tt.wantDetail) {
				t.Errorf("detail=%q, want to contain %q", detail, tt.wantDetail)
			}
			// Note must be empty iff proposals were emitted.
			note := councilProposalsNote(status, detail)
			if (status == proposalsEmitted) != (note == "") {
				t.Errorf("note=%q for status %q; want empty iff emitted", note, status)
			}
			if tt.wantDetail != "" && status == proposalsEmptyDeclared && !strings.Contains(note, tt.wantDetail) {
				t.Errorf("note %q should surface omit_reason %q", note, tt.wantDetail)
			}
		})
	}
}

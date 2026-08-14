package clients

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// councilProposalsJSON is the shape the editor's "## Backlog Proposals" block
// decodes into (.loom/163 S3). Every field is optional; a missing/malformed
// block yields nil proposals so the editor falls back to its prior markdown-only
// behavior. OmitReason lets the editor explicitly declare "nothing to decompose"
// (single unit / unmappable paths) instead of silently omitting the section,
// which is indistinguishable from the model ignoring the instruction.
type councilProposalsJSON struct {
	// Objective is the plan-level end-state + through-line the editor synthesized.
	// Optional; lifted onto EditorOutput.Objective for the Spinning-Room spin.
	Objective  string `json:"objective"`
	OmitReason string `json:"omit_reason"`
	Proposals  []struct {
		Title     string   `json:"title"`
		Priority  string   `json:"priority"`
		Labels    []string `json:"labels"`
		SpecDoc   string   `json:"spec_doc"`
		PatternID string   `json:"pattern_id"`
		Slices    []struct {
			Name               string   `json:"name"`
			Goal               string   `json:"goal"`
			Files              []string `json:"files"`
			DependsOn          []string `json:"depends_on"`
			InterfaceContracts string   `json:"interface_contracts"`
			AcceptanceCriteria string   `json:"acceptance_criteria"`
		} `json:"slices"`
	} `json:"proposals"`
}

// proposalParseStatus classifies the outcome of reading the editor's
// "## Backlog Proposals" block, so a council run that creates zero backlog items
// records WHY. Before this, created:0 was opaque — it hid the loop's
// demand-starvation (the editor writing docs but no runnable work) behind a
// number that looked the same whether the model correctly judged the brief a
// single unit, malformed the JSON, or simply ignored the instruction.
type proposalParseStatus string

const (
	proposalsEmitted       proposalParseStatus = "emitted"        // >=1 usable proposal
	proposalsMarkerAbsent  proposalParseStatus = "marker_absent"  // no "## Backlog Proposals" section
	proposalsNoJSON        proposalParseStatus = "no_json_object" // section present, no {...}
	proposalsDecodeError   proposalParseStatus = "decode_error"   // {...} failed to parse
	proposalsEmptyDeclared proposalParseStatus = "empty_declared" // valid empty list (single unit / unmappable)
	proposalsEmptyTitles   proposalParseStatus = "empty_titles"   // listed proposals but none usable
)

// parseCouncilProposalsDiag extracts the structured backlog proposals AND
// classifies the outcome. Lenient on the happy path (the same proposals the old
// parser produced), but it never silently swallows the empty case: the returned
// status + detail let the caller record why a run produced no backlog items.
func parseCouncilProposalsDiag(raw string) (proposals []council.BacklogProposal, status proposalParseStatus, detail string) {
	idx := strings.Index(raw, "## Backlog Proposals")
	if idx < 0 {
		return nil, proposalsMarkerAbsent, ""
	}
	region := raw[idx:]
	start := strings.Index(region, "{")
	end := strings.LastIndex(region, "}")
	if start < 0 || end <= start {
		return nil, proposalsNoJSON, ""
	}
	var parsed councilProposalsJSON
	if err := json.Unmarshal([]byte(region[start:end+1]), &parsed); err != nil {
		return nil, proposalsDecodeError, err.Error()
	}
	out := make([]council.BacklogProposal, 0, len(parsed.Proposals))
	for _, p := range parsed.Proposals {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			continue
		}
		bp := council.BacklogProposal{
			Title:     title,
			Labels:    p.Labels,
			Priority:  normalizePriority(p.Priority),
			SpecDoc:   strings.TrimSpace(p.SpecDoc),
			PatternID: strings.TrimSpace(p.PatternID),
		}
		for _, s := range p.Slices {
			name := strings.TrimSpace(s.Name)
			if name == "" {
				continue
			}
			bp.PlanSlices = append(bp.PlanSlices, council.PlanSliceSpec{
				Name:               name,
				Goal:               strings.TrimSpace(s.Goal),
				Files:              s.Files,
				DependsOn:          trimNonEmpty(s.DependsOn),
				InterfaceContracts: strings.TrimSpace(s.InterfaceContracts),
				AcceptanceCriteria: strings.TrimSpace(s.AcceptanceCriteria),
			})
		}
		out = append(out, bp)
	}
	if len(out) == 0 {
		// Distinguish a deliberate empty list (the model declared the work a
		// single unit or unmappable — the legitimate case) from a block that
		// listed proposals but none were usable (all empty titles — malformed).
		if len(parsed.Proposals) == 0 {
			return nil, proposalsEmptyDeclared, strings.TrimSpace(parsed.OmitReason)
		}
		return nil, proposalsEmptyTitles, ""
	}
	return out, proposalsEmitted, ""
}

// parseCouncilProposals is the lenient extractor used on the happy path: a
// missing/malformed block yields nil so the editor falls back to its prior
// markdown-only behavior. Callers that need to know WHY it was empty use
// parseCouncilProposalsDiag.
func parseCouncilProposals(raw string) []council.BacklogProposal {
	p, _, _ := parseCouncilProposalsDiag(raw)
	return p
}

// parseCouncilObjective extracts the plan-level "objective" from the same
// "## Backlog Proposals" JSON block the proposals come from (no extra model
// round-trip). Returns "" when the section, the JSON, or the key is absent or
// malformed — so an editor that omits it degrades to no objective, never an
// error. The Spinning-Room spin threads this onto the draft plan.
func parseCouncilObjective(raw string) string {
	idx := strings.Index(raw, "## Backlog Proposals")
	if idx < 0 {
		return ""
	}
	region := raw[idx:]
	start := strings.Index(region, "{")
	end := strings.LastIndex(region, "}")
	if start < 0 || end <= start {
		return ""
	}
	var parsed councilProposalsJSON
	if err := json.Unmarshal([]byte(region[start:end+1]), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Objective)
}

// trimNonEmpty trims each entry and drops the empties, so a slice's depends_on
// never carries blank/whitespace names into the plan store.
func trimNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// councilProposalsNote returns a short note for the council run Sidecar
// explaining why no backlog proposals were created, or "" when proposals were
// emitted. This turns an opaque created:0 into a diagnosable signal in the
// council_runs record and the HUD Council tab.
func councilProposalsNote(status proposalParseStatus, detail string) string {
	switch status {
	case proposalsEmitted:
		return ""
	case proposalsMarkerAbsent:
		return `no backlog proposals: editor omitted the "## Backlog Proposals" section ` +
			`(the contract asks it to always emit the section — an empty list with an ` +
			`omit_reason when there is nothing to decompose — so an absent section means ` +
			`the model did not follow the instruction)`
	case proposalsNoJSON:
		return `no backlog proposals: "## Backlog Proposals" section present but contained no JSON object`
	case proposalsDecodeError:
		return fmt.Sprintf(`no backlog proposals: "## Backlog Proposals" JSON failed to parse (%s)`, detail)
	case proposalsEmptyDeclared:
		if detail != "" {
			return fmt.Sprintf("no backlog proposals: editor declared an empty list (omit_reason: %s)", detail)
		}
		return "no backlog proposals: editor declared an empty list (no omit_reason given)"
	case proposalsEmptyTitles:
		return "no backlog proposals: JSON listed proposals but none had a usable title"
	default:
		return ""
	}
}

// normalizePriority maps a free-form priority string to a valid store.Priority,
// defaulting to P2 when absent or unrecognized.
func normalizePriority(raw string) store.Priority {
	switch store.Priority(strings.ToUpper(strings.TrimSpace(raw))) {
	case store.P0:
		return store.P0
	case store.P1:
		return store.P1
	case store.P3:
		return store.P3
	default:
		return store.P2
	}
}

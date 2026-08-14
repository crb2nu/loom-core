package council

import (
	"fmt"
	"strings"
)

// EditorGuardrailsPromptSection is appended to the council editor prompt so
// every backend receives the same incident/remediation contract.
//
// It is the single seam through which package-local prompt contracts reach the
// STABLE (cacheable) half of the editor prompt — clients.buildCouncilEditorPromptParts
// writes it once, ahead of the memory block, repo layout, and the per-run
// brief. Sections joined here MUST be constant: any per-item variation would
// invalidate the Anthropic backend's cached prefix on every run.
//
// SliceScopePromptSection joined 2026-07-26 (see slice_scope_rules.go): the
// scope gate escalated three items in one afternoon because the editor declared
// a slice's primary directory and omitted the shared components the work
// necessarily reached.
func EditorGuardrailsPromptSection() string {
	return ExternalIncidentPlanningRulesPromptSection() + SliceScopePromptSection()
}

// EditorGuardrailOutcome records deterministic post-processing applied to an
// editor output before backlog mutation.
type EditorGuardrailOutcome struct {
	ExternalDependencyIncident bool
	LabelsAdded                int
	ExternalOnlyDropped        int
	OmitReason                 string
	Incident                   ExternalIncidentPlanningDecision
}

// Applied reports whether the guard changed or classified the output.
func (o EditorGuardrailOutcome) Applied() bool {
	return o.ExternalDependencyIncident || o.LabelsAdded > 0 || o.ExternalOnlyDropped > 0 || o.OmitReason != ""
}

// Note returns the compact sidecar note for an applied guardrail outcome.
func (o EditorGuardrailOutcome) Note() string {
	if !o.Applied() {
		return ""
	}
	parts := []string{"editor guardrails: external dependency incident classified"}
	if o.Incident.Class != "" {
		parts = append(parts, fmt.Sprintf("class=`%s`", o.Incident.Class))
	}
	if o.Incident.Dependency != "" {
		parts = append(parts, fmt.Sprintf("external=`%s`", o.Incident.Dependency))
	}
	if o.Incident.Evidence != "" {
		parts = append(parts, fmt.Sprintf("evidence=`%s`", truncateGuardrailNote(o.Incident.Evidence, 160)))
	}
	if o.ExternalOnlyDropped > 0 {
		parts = append(parts, pluralize(o.ExternalOnlyDropped, "external-only proposal", "external-only proposals")+" dropped")
	}
	if o.LabelsAdded > 0 {
		parts = append(parts, pluralize(o.LabelsAdded, "proposal", "proposals")+" labeled")
	}
	if o.OmitReason != "" {
		parts = append(parts, "omit_reason: "+o.OmitReason)
	}
	return strings.Join(parts, "; ")
}

// ApplyEditorGuardrails mutates out in place. It is intentionally conservative:
// repo-scoped proposals are preserved, but external-remediation proposals with
// no file-backed in-repo guardrail/docs/telemetry/config action are removed
// before the mutator can create work the pipeline cannot complete.
func ApplyEditorGuardrails(out *EditorOutput) EditorGuardrailOutcome {
	if out == nil {
		return EditorGuardrailOutcome{}
	}
	text := editorOutputText(out)
	decision := ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{Body: text})
	incident := decision.Class == CIIncidentExternalDependency || isExternalDependencyIncidentText(text)
	if incident && decision.Class != CIIncidentExternalDependency {
		decision = externalIncidentDecision("", firstExternalIncidentEvidenceLine(text), "evidence describes an external dependency incident")
	}
	outcome := EditorGuardrailOutcome{ExternalDependencyIncident: incident, Incident: decision}
	if len(out.BacklogProposals) == 0 {
		if incident {
			setExternalIncidentOmitReason(out, &outcome)
		}
		return outcome
	}

	kept := out.BacklogProposals[:0]
	for _, p := range out.BacklogProposals {
		if isNonActionableExternalProposal(p, incident) {
			outcome.ExternalOnlyDropped++
			continue
		}
		if incident || isCanonicalExternalIncidentText(proposalText(p)) {
			if !hasLabel(p.Labels, ExternalDependencyIncidentLabel) {
				p.Labels = append(p.Labels, ExternalDependencyIncidentLabel)
				outcome.LabelsAdded++
			}
		}
		kept = append(kept, p)
	}
	out.BacklogProposals = kept
	out.Sidecar.BacklogDeltas.Created = len(kept)
	if len(kept) == 0 && (incident || outcome.ExternalOnlyDropped > 0) {
		outcome.ExternalDependencyIncident = true
		setExternalIncidentOmitReason(out, &outcome)
	}
	return outcome
}

func setExternalIncidentOmitReason(out *EditorOutput, outcome *EditorGuardrailOutcome) {
	if out == nil || outcome == nil {
		return
	}
	out.Sidecar.OmitReason = ExternalIncidentNoInRepoFollowUpReason
	outcome.OmitReason = ExternalIncidentNoInRepoFollowUpReason
}

func truncateGuardrailNote(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func isNonActionableExternalProposal(p BacklogProposal, incident bool) bool {
	hasFiles := proposalHasFiles(p)
	if hasFiles && (!incident || isAllowedExternalIncidentFollowUp(p)) {
		return false
	}
	txt := strings.ToLower(proposalText(p))
	if containsAny(txt, externalDependencyTerms) && containsAny(txt, externalRemediationTerms) {
		return true
	}
	return incident
}

func isAllowedExternalIncidentFollowUp(p BacklogProposal) bool {
	txt := strings.ToLower(proposalText(p))
	if containsAny(txt, forbiddenExternalIncidentFollowUpTerms) {
		return false
	}
	if containsAnyDelimited(txt, allowedExternalIncidentFollowUpTerms) {
		return true
	}
	return proposalFilesMatchAllowedExternalIncidentFollowUp(p)
}

func proposalHasFiles(p BacklogProposal) bool {
	for _, s := range p.PlanSlices {
		if hasNonEmptyString(s.Files) {
			return true
		}
	}
	for _, s := range p.Slices {
		if hasNonEmptyString(s.Files) {
			return true
		}
	}
	return false
}

func proposalFilesMatchAllowedExternalIncidentFollowUp(p BacklogProposal) bool {
	for _, s := range p.PlanSlices {
		if filesMatchAllowedExternalIncidentFollowUp(s.Files) {
			return true
		}
	}
	for _, s := range p.Slices {
		if filesMatchAllowedExternalIncidentFollowUp(s.Files) {
			return true
		}
	}
	return false
}

func filesMatchAllowedExternalIncidentFollowUp(files []string) bool {
	for _, file := range files {
		if isAllowedExternalIncidentFollowUpFile(file) {
			return true
		}
	}
	return false
}

func isAllowedExternalIncidentFollowUpFile(file string) bool {
	path := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(file, "\\", "/")))
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "docs/") || path == "readme.md" || path == "changelog.md" || path == "roadmap.md" || path == "agents.md" {
		return true
	}
	if strings.Contains(path, "guardrail") || strings.Contains(path, "telemetry") || strings.Contains(path, "otel") || strings.Contains(path, "metric") {
		return true
	}
	if strings.Contains(path, "config") || strings.Contains(path, "policy") || strings.Contains(path, "registry") {
		return true
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".toml") {
		return true
	}
	return false
}

func editorOutputText(out *EditorOutput) string {
	var b strings.Builder
	b.WriteString(out.Sidecar.Notes)
	for _, d := range out.Documents {
		b.WriteByte('\n')
		b.WriteString(d.Title)
		b.WriteByte('\n')
		b.WriteString(d.Body)
	}
	for _, p := range out.BacklogProposals {
		b.WriteByte('\n')
		b.WriteString(proposalText(p))
	}
	return b.String()
}

func proposalText(p BacklogProposal) string {
	var b strings.Builder
	b.WriteString(p.Title)
	b.WriteByte('\n')
	b.WriteString(p.SpecDoc)
	b.WriteByte('\n')
	b.WriteString(p.SpecAnchor)
	b.WriteByte('\n')
	b.WriteString(p.PatternID)
	b.WriteByte('\n')
	b.WriteString(p.Notes)
	for _, label := range p.Labels {
		b.WriteByte('\n')
		b.WriteString(label)
	}
	for _, s := range p.PlanSlices {
		b.WriteByte('\n')
		b.WriteString(s.Name)
		b.WriteByte('\n')
		b.WriteString(s.Goal)
	}
	for _, s := range p.Slices {
		b.WriteByte('\n')
		b.WriteString(s.Name)
		for _, test := range s.Tests {
			b.WriteByte('\n')
			b.WriteString(test)
		}
	}
	return b.String()
}

func isExternalDependencyIncidentText(raw string) bool {
	txt := strings.ToLower(raw)
	return containsAny(txt, externalDependencyTerms) && containsAny(txt, incidentTerms)
}

func isCanonicalExternalIncidentText(raw string) bool {
	if ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{Body: raw}).Class == CIIncidentExternalDependency {
		return true
	}
	return isExternalDependencyIncidentText(raw)
}

func containsAny(txt string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(txt, term) {
			return true
		}
	}
	return false
}

func containsAnyDelimited(txt string, terms []string) bool {
	for _, term := range terms {
		if containsDelimited(txt, term) {
			return true
		}
	}
	return false
}

func containsDelimited(txt, term string) bool {
	term = strings.TrimSpace(strings.ToLower(term))
	if term == "" {
		return false
	}
	for start := 0; ; {
		idx := strings.Index(txt[start:], term)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(term)
		if (idx == 0 || !isTermChar(txt[idx-1])) && (end == len(txt) || !isTermChar(txt[end])) {
			return true
		}
		start = idx + 1
	}
}

func isTermChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

func hasNonEmptyString(in []string) bool {
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

var externalDependencyTerms = []string{
	"external dependency",
	"external service",
	"third-party",
	"third party",
	"third-party api",
	"third party api",
	"external api",
	"provider",
	"openai",
	"flexinfer",
	"gitlab",
	"github",
	"kubernetes",
	"k8s",
	"registry",
	"container registry",
	"network",
	"storage",
	"api outage",
	"api failure",
	"rate limit",
	"quota",
	"upstream",
}

var incidentTerms = []string{
	"incident",
	"outage",
	"failure",
	"failed",
	"degraded",
	"blocked",
	"error",
	"errors",
	"dependency failure",
	"unavailable",
	"timeout",
	"timed out",
	"rate limit",
	"quota",
	"5xx",
	"502",
	"503",
	"504",
}

var externalRemediationTerms = []string{
	"remediate",
	"restore",
	"restart",
	"reconcile",
	"raise",
	"increase",
	"change",
	"rotate",
	"replace",
	"fix",
	"rerun",
	"re-run",
	"retry",
	"until green",
	"contact",
	"ask",
	"escalate",
	"ticket",
	"open support",
	"support ticket",
	"provider quota",
	"credentials",
	"credential",
	"external api",
	"external service",
}

var allowedExternalIncidentFollowUpTerms = []string{
	"backoff",
	"classify",
	"classification",
	"classifier",
	"config",
	"configuration",
	"docs",
	"documentation",
	"guardrail",
	"incident triage",
	"log",
	"logging",
	"metric",
	"metrics",
	"observability",
	"otel",
	"policy",
	"retry guardrail",
	"runbook",
	"telemetry",
}

var forbiddenExternalIncidentFollowUpTerms = []string{
	"change credential",
	"change credentials",
	"contact provider",
	"contact support",
	"grant quota",
	"increase quota",
	"open support ticket",
	"provider quota",
	"re-run",
	"rerun",
	"restart",
	"restart external service",
	"restart gitlab",
	"restart runner",
	"restore external service",
	"rotate",
	"rotate credential",
	"rotate credentials",
	"wait for",
	"until green",
	"wait for gitlab",
	"wait for provider",
	"wait for registry",
}

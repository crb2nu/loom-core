package council

import (
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mcperror"
)

const (
	// ExternalEscalationRunbookPath is the local operator guidance linked from
	// external dependency escalations. Keep this repo-local so an incident issue
	// always routes humans to actionable Mills guidance, not a guessed upstream
	// remediation.
	ExternalEscalationRunbookPath = "docs/mills-escalation-and-dependency-failures.md"
)

// ExternalEscalationRenderInput is the evidence available when rendering a
// human escalation artifact.
type ExternalEscalationRenderInput struct {
	Reason      string
	LastLogTail string
}

// ExternalEscalationRender is the deterministic external-dependency rendering
// result. Incident carries stable classifier metadata for tests and callers
// that need to persist or inspect the classification.
type ExternalEscalationRender struct {
	Incident mcperror.ExternalIncident
	Markdown string
}

// FormatExternalEscalation renders a clear external-dependency block for known
// incident signatures. Unknown evidence returns ok=false so normal code,
// config, and infrastructure failures are not labeled speculatively.
func FormatExternalEscalation(input ExternalEscalationRenderInput) (ExternalEscalationRender, bool) {
	incident, ok := mcperror.ClassifyExternalIncident(externalEscalationEvidenceText(input))
	if !ok {
		return ExternalEscalationRender{}, false
	}

	var b strings.Builder
	b.WriteString("### External dependency incident\n\n")
	b.WriteString("This escalation matched a known external dependency signature. Treat it as dependency recovery work unless the linked runbook identifies a local follow-up.\n\n")
	fmt.Fprintf(&b, "- **Incident class**: `%s`\n", CIIncidentExternalDependency)
	fmt.Fprintf(&b, "- **Disposition**: `%s`\n", CIIncidentDispositionWaitDependency)
	fmt.Fprintf(&b, "- **Dependency**: `%s`\n", incident.Dependency)
	fmt.Fprintf(&b, "- **Signature**: `%s`\n", incident.ID)
	fmt.Fprintf(&b, "- **Summary**: %s\n", singleLine(incident.Summary))
	fmt.Fprintf(&b, "- **Local action**: `%s`\n", "follow_external_dependency_runbook")
	fmt.Fprintf(&b, "- **Runbook**: `%s`\n", ExternalEscalationRunbookPath)
	if incident.Evidence != "" {
		fmt.Fprintf(&b, "- **Evidence**: `%s`\n", singleLine(incident.Evidence))
	}
	b.WriteString("\nLocal guidance:\n")
	b.WriteString("- Do not create speculative in-repo remediation work for the upstream dependency failure.\n")
	b.WriteString("- Use the runbook to decide whether to wait, retry after recovery, or add a local classifier/retry/docs/telemetry/config improvement.\n")
	b.WriteString("- Keep this escalation issue as the local incident record and link any upstream incident or dependency-owner note back here.\n")

	return ExternalEscalationRender{Incident: incident, Markdown: b.String()}, true
}

func externalEscalationEvidenceText(input ExternalEscalationRenderInput) string {
	var parts []string
	if s := strings.TrimSpace(input.Reason); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(input.LastLogTail); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

func singleLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return strings.ReplaceAll(s, "`", "'")
}

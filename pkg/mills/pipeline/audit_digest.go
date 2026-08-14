package pipeline

import "fmt"

// AuditDigestLabel is the GitLab label every rolling audit-advisory digest
// issue carries. The matcher lists open issues with this label before scanning
// bodies for AuditDigestMarker, so the lookup stays a single narrow query
// (mirrors how the escalator scopes to the `mills-escalation` label).
const AuditDigestLabel = "audit-digest"

// AuditDigestMarker returns the stable, format-independent marker embedded in
// the body of a rolling audit-advisory digest issue. The audit follow-up writer
// opens at most one digest issue per UTC calendar day (period is "YYYY-MM-DD");
// the marker lets the writer — and any matcher that scans issue bodies for it —
// find that day's open digest and append to it rather than opening a fresh
// issue per advisory finding.
//
// It lives beside EscalationDedupMarker because pkg/mills/pipeline is the shared
// home of the issue-dedup contract: both pkg/mills/audit (the producer) and
// pkg/mills/clients (the GitLab matcher) already import this package, so the
// marker format cannot drift between the two.
func AuditDigestMarker(period string) string {
	return fmt.Sprintf("<!-- mills-audit-digest:period=%s -->", period)
}

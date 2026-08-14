package audit

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// AuditAdvisoryDefaultStaleAfter is the conservative age used by the one-time
// cleanup unless an operator supplies an explicit cutoff.
const AuditAdvisoryDefaultStaleAfter = 30 * 24 * time.Hour

// AuditAdvisoryDigestAuthor is the exact GitLab username expected on digest
// issues. Deployments using a different bot identity must pass -author.
const AuditAdvisoryDigestAuthor = "mills-bot"

// IsAuditAdvisoryDigest reports whether issue has the complete producer
// identity. A title fragment, marker, label, or author by itself is never
// enough to authorize mutation.
func IsAuditAdvisoryDigest(issue DigestIssue, author string) bool {
	if issue.IID <= 0 || issue.State != "opened" || issue.Author != author ||
		!hasLabel(issue.Labels, AuditAdvisoryDigestLabel) ||
		!strings.HasPrefix(issue.Title, AuditAdvisoryDigestTitlePrefix) ||
		!strings.HasSuffix(issue.Title, AuditAdvisoryDigestTitleSuffix) {
		return false
	}

	period := strings.TrimSuffix(strings.TrimPrefix(issue.Title, AuditAdvisoryDigestTitlePrefix), AuditAdvisoryDigestTitleSuffix)
	if _, err := time.Parse("2006-01-02", period); err != nil {
		return false
	}
	return strings.Contains(issue.Description, pipeline.AuditDigestMarker(period))
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

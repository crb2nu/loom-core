package council

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// ExternalIncidentBannerHeading is stable so HUD and API consumers can
	// locate the section without parsing operator-facing prose.
	ExternalIncidentBannerHeading = "External dependency incidents"

	externalIncidentRunbookPath = "docs/runbook-external-dependency-incidents.md"
)

// RenderExternalIncidentBanner turns the external-incident threshold verdict
// into a compact council-brief section. The verdict is copied and normalized;
// rendering never mutates the policy result supplied by the caller.
func RenderExternalIncidentBanner(verdict PolicyVerdict) BriefSection {
	reasons := normalizeBannerReasons(verdict.Reasons)
	var b strings.Builder

	if verdict.Pass {
		b.WriteString("> **Auto-merge not suppressed by this guardrail.** External-dependency incidents are within the configured threshold.\n\n")
	} else {
		b.WriteString("> **Auto-merge suppressed.** External-dependency incidents exceeded the configured threshold.\n\n")
	}

	fmt.Fprintf(&b, "- **Verdict code**: `%s`\n", bannerValue(verdict.Code, "unspecified"))
	fmt.Fprintf(&b, "- **Severity**: `%s`\n", bannerValue(string(verdict.Severity), string(PolicySeverityWarning)))
	if len(reasons) == 0 {
		b.WriteString("- **Structured reason**: `none reported`\n")
	} else {
		for _, reason := range reasons {
			fmt.Fprintf(&b, "- **Structured reason**: `%s`\n", inlineCode(reason))
		}
	}
	if observed, ok := verdict.Metrics["external_incident_clusters"]; ok {
		fmt.Fprintf(&b, "- **Observed clusters**: `%g`\n", observed)
	}
	if threshold, ok := verdict.Metrics["external_incident_threshold"]; ok {
		fmt.Fprintf(&b, "- **Policy threshold**: `%g`\n", threshold)
	}

	if verdict.Pass {
		b.WriteString("- **Operator action**: Continue monitoring; this verdict does not override other auto-merge guardrails.\n")
	} else {
		fmt.Fprintf(&b, "- **Operator action**: Triage and verify recovery before restoring auto-merge. Follow `%s`; use its manual-override procedure only for an explicitly accepted risk.\n", externalIncidentRunbookPath)
	}

	return BriefSection{Heading: ExternalIncidentBannerHeading, Body: b.String()}
}

func normalizeBannerReasons(reasons []string) []string {
	out := make([]string, 0, len(reasons))
	seen := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func bannerValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return inlineCode(value)
}

func inlineCode(value string) string {
	return strings.NewReplacer("`", "'", "\r", " ", "\n", " ").Replace(strings.TrimSpace(value))
}

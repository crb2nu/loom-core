package pipeline

import (
	"strings"
	"testing"
)

func TestAuditDigestMarker_StableAndPeriodScoped(t *testing.T) {
	got := AuditDigestMarker("2026-07-20")
	if want := "<!-- mills-audit-digest:period=2026-07-20 -->"; got != want {
		t.Fatalf("AuditDigestMarker = %q, want %q", got, want)
	}
	// Distinct periods must yield distinct markers so a body-scan matches only
	// the intended day's digest (the find-or-create contract depends on this).
	if AuditDigestMarker("2026-07-20") == AuditDigestMarker("2026-07-21") {
		t.Fatal("markers for different periods must differ")
	}
	// Kept as an HTML comment so it renders invisibly in the issue body, like
	// the escalation markers.
	if !strings.HasPrefix(got, "<!--") || !strings.HasSuffix(got, "-->") {
		t.Errorf("marker must be an HTML comment: %q", got)
	}
}

func TestAuditDigestMarker_MatchesInBody(t *testing.T) {
	body := "Rolling digest ...\n\n" + AuditDigestMarker("2026-07-20") + "\n\n---\n"
	if !strings.Contains(body, AuditDigestMarker("2026-07-20")) {
		t.Error("marker must be substring-matchable in a rendered body")
	}
	if strings.Contains(body, AuditDigestMarker("2026-07-19")) {
		t.Error("a body must not match a different period's marker")
	}
}

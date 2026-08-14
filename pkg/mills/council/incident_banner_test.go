package council

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenderExternalIncidentBannerSuppressed(t *testing.T) {
	verdict := PolicyVerdict{
		Pass:     false,
		Code:     "external_incident_threshold_exceeded",
		Severity: PolicySeverityCritical,
		Action:   PolicyActionEscalate,
		Reasons:  []string{"threshold exceeded for ref main", "3 incidents in 24h", "threshold exceeded for ref main"},
		Metrics: map[string]float64{
			"external_incident_clusters":  4,
			"external_incident_threshold": 3,
		},
	}
	originalReasons := append([]string(nil), verdict.Reasons...)

	got := RenderExternalIncidentBanner(verdict)

	if got.Heading != ExternalIncidentBannerHeading {
		t.Fatalf("heading = %q", got.Heading)
	}
	for _, want := range []string{
		"**Auto-merge suppressed.**",
		"`external_incident_threshold_exceeded`",
		"`critical`",
		"**Observed clusters**: `4`",
		"**Policy threshold**: `3`",
		externalIncidentRunbookPath,
		"manual-override procedure",
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("banner missing %q:\n%s", want, got.Body)
		}
	}
	if strings.Count(got.Body, "threshold exceeded for ref main") != 1 {
		t.Fatalf("reasons were not deduplicated:\n%s", got.Body)
	}
	if !reflect.DeepEqual(verdict.Reasons, originalReasons) {
		t.Fatalf("renderer mutated verdict reasons: got %v want %v", verdict.Reasons, originalReasons)
	}
	if again := RenderExternalIncidentBanner(verdict); !reflect.DeepEqual(got, again) {
		t.Fatalf("render is not deterministic:\nfirst=%+v\nagain=%+v", got, again)
	}
}

func TestRenderExternalIncidentBannerNotSuppressed(t *testing.T) {
	got := RenderExternalIncidentBanner(PolicyVerdict{
		Pass:     true,
		Code:     "ok",
		Severity: PolicySeverityOK,
		Action:   PolicyActionNone,
	})

	for _, want := range []string{
		"**Auto-merge not suppressed by this guardrail.**",
		"within the configured threshold",
		"this verdict does not override other auto-merge guardrails.",
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("banner missing %q:\n%s", want, got.Body)
		}
	}
	if strings.Contains(got.Body, "**Auto-merge suppressed.**") {
		t.Fatalf("passing verdict rendered suppression:\n%s", got.Body)
	}
}

func TestCompilePlacesExternalIncidentBannerFirst(t *testing.T) {
	st := newCouncilTestStore(t)
	verdict := PolicyVerdict{
		Pass:     false,
		Code:     "external_incident_threshold_exceeded",
		Severity: PolicySeverityCritical,
		Reasons:  []string{"incident cluster count exceeded threshold"},
	}

	brief, err := Compile(context.Background(), BriefSources{
		Store:                   st,
		Now:                     func() time.Time { return time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC) },
		ExternalIncidentVerdict: &verdict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Sections) == 0 || brief.Sections[0].Heading != ExternalIncidentBannerHeading {
		t.Fatalf("first section = %+v, want incident banner", brief.Sections)
	}
	bannerAt := strings.Index(brief.Markdown, "## "+ExternalIncidentBannerHeading)
	intentsAt := strings.Index(brief.Markdown, "## Roadmap intents")
	if bannerAt < 0 || intentsAt < 0 || bannerAt > intentsAt {
		t.Fatalf("banner is not prominent in compiled brief:\n%s", brief.Markdown)
	}
}

package council

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// The store persists the pipeline's failure vocabulary, not the incident
// taxonomy. Every spelling that reaches those columns must land on a real
// CIIncidentClass constant — returning the raw string produced classes like
// "code" that no constant matches, so every taxonomy comparison downstream
// silently missed.
func TestCanonicalClassifiedCIFailureClass_MapsStoreVocabularyOntoTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		summary *store.ClassifiedCIFailureSummary
		want    CIIncidentClass
	}{
		{"nil summary", nil, CIIncidentUnclassified},
		{"external dependency name wins", &store.ClassifiedCIFailureSummary{
			ExternalDependency: "gitlab.com", FailureClass: "code",
		}, CIIncidentExternalDependency},
		{"external dependency id wins", &store.ClassifiedCIFailureSummary{
			ExternalDependencyID: "dep-1",
		}, CIIncidentExternalDependency},
		{"code", &store.ClassifiedCIFailureSummary{FailureClass: "code"}, CIIncidentRepositoryRegression},
		{"store spelling: configuration", &store.ClassifiedCIFailureSummary{
			FailureClass: "configuration",
		}, CIIncidentCIConfiguration},
		{"pipeline spelling: config", &store.ClassifiedCIFailureSummary{
			FailureClass: "config",
		}, CIIncidentCIConfiguration},
		{"store spelling: infrastructure", &store.ClassifiedCIFailureSummary{
			FailureClass: "infrastructure",
		}, CIIncidentRunnerInfrastructure},
		{"pipeline spelling: infra", &store.ClassifiedCIFailureSummary{
			FailureClass: "infra",
		}, CIIncidentRunnerInfrastructure},
		{"transient", &store.ClassifiedCIFailureSummary{
			FailureClass: "transient",
		}, CIIncidentFlakeOrTransient},
		{"transient_quota", &store.ClassifiedCIFailureSummary{
			FailureClass: "transient_quota",
		}, CIIncidentFlakeOrTransient},
		{"case and spacing tolerated", &store.ClassifiedCIFailureSummary{
			FailureClass: "  INFRA ",
		}, CIIncidentRunnerInfrastructure},
		{"falls through to escalation class", &store.ClassifiedCIFailureSummary{
			EscalationClass: "infra",
		}, CIIncidentRunnerInfrastructure},
		{"unrecognized failure class falls through to escalation class", &store.ClassifiedCIFailureSummary{
			FailureClass: "something-new", EscalationClass: "code",
		}, CIIncidentRepositoryRegression},
		{"wholly unrecognized is unclassified, never a raw string", &store.ClassifiedCIFailureSummary{
			FailureClass: "something-new",
		}, CIIncidentUnclassified},
		{"empty", &store.ClassifiedCIFailureSummary{}, CIIncidentUnclassified},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalClassifiedCIFailureClass(tc.summary); got != tc.want {
				t.Fatalf("canonicalClassifiedCIFailureClass() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Whatever the mapping returns must be a member of the closed taxonomy —
// that is the property the old raw-string fallback violated.
func TestCanonicalClassifiedCIFailureClass_AlwaysReturnsAKnownClass(t *testing.T) {
	known := map[CIIncidentClass]bool{
		CIIncidentRepositoryRegression: true,
		CIIncidentCIConfiguration:      true,
		CIIncidentRunnerInfrastructure: true,
		CIIncidentExternalDependency:   true,
		CIIncidentDependencyUpdate:     true,
		CIIncidentFlakeOrTransient:     true,
		CIIncidentBranchOrPlanHygiene:  true,
		CIIncidentUnclassified:         true,
	}
	for _, raw := range []string{"", "code", "infra", "configuration", "transient", "wat", "  ", "EXTERNAL"} {
		got := canonicalClassifiedCIFailureClass(&store.ClassifiedCIFailureSummary{FailureClass: raw})
		if !known[got] {
			t.Errorf("failure class %q produced non-taxonomy class %q", raw, got)
		}
	}
}

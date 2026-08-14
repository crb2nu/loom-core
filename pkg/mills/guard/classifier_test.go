package guard

import "testing"

func TestClassifyFailureExternalDependencySignatures(t *testing.T) {
	tests := []string{
		"connect ECONNREFUSED 10.0.0.4:6379",
		"clickhouse: MergeTreeBackgroundExecutor failed",
		"LONGHORN manager: NO   AVAILABLE-DISK for replica",
		"upstream CIRCUIT_BREAKER_OPEN",
	}
	for _, evidence := range tests {
		t.Run(evidence, func(t *testing.T) {
			got, matched := ClassifyFailure(evidence)
			if !matched || got != ExternalDependencyIncident {
				t.Fatalf("ClassifyFailure() = %q, %t; want %q, true", got, matched, ExternalDependencyIncident)
			}
		})
	}
}

func TestClassifyFailureUnknownFailsClosed(t *testing.T) {
	if got, matched := ClassifyFailure("application assertion failed"); matched || got != "" {
		t.Fatalf("ClassifyFailure() = %q, %t; want empty, false", got, matched)
	}
	if _, matched := ClassifyFailure("no available disk in local cache"); matched {
		t.Fatal("Longhorn capacity signature matched without Longhorn attribution")
	}
}
